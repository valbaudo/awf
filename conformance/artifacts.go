package conformance

import (
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testArtifacts is the SP1 artifact-channel bucket. Its single sub-test
// (cross_container_handoff_and_resume) pins the headline durability claim of the
// feature — "content-addressed, resume-safe" handoff of a NAMED output_files
// artifact from one step's container into a LATER step's DISTINCT container via
// input_files (the man page's "Artifact channel"). The sub-test asserts three
// things in one crash-then-resume flow:
//
//   - artifact OUT: the producer's exec writes /out/report.md; the committed
//     NodeResult.Files[path] points at a CAS blob whose bytes are the sentinel —
//     output_files capture went through Blobs (content-addressed).
//   - cross-container handoff: the consumer `hunt` runs in a DISTINCT container
//     `box` (the sentinel was seeded ONLY into `lab`) and commits ok — which
//     means resolve→Blobs.Get→CopyTo-into-box all succeeded. (The byte-exact
//     "box received these bytes" proof is the engine dispatcher test on a
//     directly-held handle; the fake's scripted exec can't read the staged file
//     to echo it, so this bucket proves the WIRING, not the bytes.)
//   - resume re-stages from the committed ref: run 1 CRASHES `hunt`'s exec AFTER
//     `recon` has committed (artifact blob durable, consumer uncommitted). On
//     resume, committed `recon` is folded and SKIPPED — re-running it would error
//     against the resume fake, which does NOT program `recon.sh`; and `hunt` (the
//     uncommitted frontier) RE-RESOLVES its input_files from `recon`'s committed
//     CAS ref in the SURVIVING Blobs store, re-stages via CopyTo, and commits ok.
//     This is the resume-safety proof: the consumer re-stages from the
//     content-addressed ref, not from live container state (recon's container is
//     gone — a fresh fake backs the resume).
//
// FAKE-ONLY: the fixture programs the fake's exec-produces-files affordance
// (container.Fake.ProgramExecWithFiles) so the producer makes its own artifact
// with no harness-lifecycle seam. A real Docker backend has no scripted-exec
// equivalent here; skip cleanly.
func testArtifacts(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("artifacts bucket programs the fake's ProgramExecWithFiles affordance; fake-only")
	}
	t.Run("cross_container_handoff_and_resume", func(t *testing.T) {
		testArtifactsCrossContainerAndResume(t, factory)
	})
}

func testArtifactsCrossContainerAndResume(t *testing.T, factory BackendFactory) {
	t.Helper()

	sentinel := []byte("recon findings\n")

	// Round 1 factory: `recon.sh` PRODUCES /out/report.md (the artifact, via the
	// fake's exec-produces-files affordance — SP1 Task 8a) and `hunt.sh` is
	// programmed too, but the run fake CRASHES the 2nd exec (FailExecAfterN(1)).
	// Exec order is recon (#0, succeeds → recon commits with a captured artifact
	// ref) then hunt (#1, crashes → hunt never commits). retry: { attempts: 1 }
	// on both steps (fixture) makes the one-shot fault actually halt hunt rather
	// than be recovered by the default 3 attempts. A custom closure records the
	// run/resume fakes so the resume fake omits `recon.sh` (proving recon is
	// skipped, not re-run). Mirrors the snapshot bucket's run/resume split.
	var runFake, resumeFake *container.Fake
	h := newHarness(t, func() container.Backend {
		f := container.NewFake()
		if runFake == nil {
			f.ProgramExecWithFiles("./recon.sh", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/report.md": sentinel})
			f.ProgramExec("./hunt.sh", container.ExecResult{ExitCode: 0}, nil)
			f.FailExecAfterN(1) // crash the 2nd exec (hunt) — recon already committed
			runFake = f
		} else {
			// Resume fake: program ONLY hunt.sh. If resume re-ran recon it would
			// error here ("no programmed result for ./recon.sh"); if hunt failed to
			// re-fetch the committed artifact from the surviving Blobs store, hunt
			// would not commit. Either way the resume assertion catches it.
			f.ProgramExec("./hunt.sh", container.ExecResult{ExitCode: 0}, nil)
			resumeFake = f
		}
		return f
	}, artifactChannelWorkflow)

	// Run 1 crashes hunt: recon commits (with a captured artifact ref); hunt
	// fails its single attempt and propagates a non-ok outcome.
	oc, _ := h.runWorkflow(t)
	if oc == "" {
		t.Fatal("first run produced no outcome (harness error before the workflow evaluated)")
	}
	if oc == engine.OutcomeOK {
		t.Fatal("first run unexpectedly ok — FailExecAfterN(1) did not crash hunt (check retry: {attempts: 1} + the fake's one-shot semantic)")
	}

	// Fold the committed log to inspect the producer's recorded artifact ref.
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	// (1) Artifact OUT: recon's committed NodeResult.Files maps the declared
	// path to a CAS ref, and that blob's bytes are the sentinel — output_files
	// capture went through Blobs (content-addressed).
	recon, ok := rs.Completed["recon"]
	if !ok {
		t.Fatal("recon not committed")
	}
	reconRef, ok := recon.Files["/out/report.md"]
	if !ok {
		t.Fatalf("recon.Files missing /out/report.md; got %v", recon.Files)
	}
	gotBytes, err := h.blobs.Get(reconRef)
	if err != nil {
		t.Fatalf("Blobs.Get(%q): %v", reconRef, err)
	}
	if string(gotBytes) != string(sentinel) {
		t.Errorf("committed artifact bytes = %q, want %q", gotBytes, sentinel)
	}

	// hunt must NOT have committed in run 1 (it crashed) — otherwise resume would
	// fold-and-skip it and never re-resolve input_files, voiding assertion (3).
	if _, ok := rs.Completed["hunt"]; ok {
		t.Fatal("hunt committed in run 1; the crash setup is wrong (resume would skip hunt, voiding the re-stage proof)")
	}

	// (2)+(3) Resume re-stages from the committed CAS ref. Committed `recon` is
	// folded and SKIPPED (the resume fake omits `recon.sh`); `hunt` — the
	// uncommitted frontier, in a DISTINCT container `box` backed by a FRESH fake
	// — re-resolves input_files from recon's committed ref in the surviving Blobs
	// store, re-stages via CopyTo, and commits ok. This is the cross-container,
	// resume-safe handoff: the consumer re-stages from the content-addressed
	// artifact, never from live container state.
	oc2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v (committed recon must be skipped; hunt must re-fetch from the committed blob)", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", oc2)
	}
	if resumeFake == nil {
		t.Fatal("resume did not mint a second fake")
	}

	// Re-fold and confirm hunt committed ok on resume (the re-staged frontier).
	rs2, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold after resume: %v", err)
	}
	if _, ok := rs2.Completed["hunt"]; !ok {
		t.Fatal("hunt not committed after resume — re-resolve→Get→CopyTo-into-box failed on the uncommitted frontier")
	}
}
