package conformance

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// snapshotWorkspaceWorkflow — code steps across a snapshot:workspace container
// (ws) and a plain (non-snapshot) container (plain). step2's exec is crashed so
// step1 commits (capturing a snapshot_ref) and the frontier (step2, step3)
// re-executes on resume — at which point resume must RESTORE ws from step1's
// snapshot, not Create an empty one.
//
//   - step1 (ws): PRODUCES /work/state.txt (via the fake's ProgramExecWithFiles
//     affordance). At commit, the fake serializes ws's whole fs — including this
//     file — into the snapshot blob. This is the content the snapshot carries.
//   - step2 (ws): the frontier. On resume it runs against the RESTORED ws handle
//     and declares output_files { state: /work/state.txt }, so the dispatcher's
//     post-exec CaptureFiles reads /work/state.txt off the restored handle. If
//     Restore did not round-trip the file, CaptureFiles errors ("path not
//     present") and step2 never commits — so the content assertion is real, not
//     cosmetic. The committed step2.Files[/work/state.txt] blob bytes must equal
//     what step1 wrote: proof the snapshot survived run→resume.
//   - step3 (plain): the NEGATIVE case. `plain` has no snapshot: workspace, so it
//     is Created fresh on resume — never Restored. The bucket asserts the resume
//     fake's RestoreCalls contains ws but NOT plain (mirror-Docker policy:
//     snapshot is opt-in; non-snapshot containers Create fresh).
//
// retry: { attempts: 1 } on every step pins the one-shot FailExecAfterN fault so
// it actually halts step2 instead of being recovered by the default 3 attempts
// (mirrors fiveStepSeqWorkflow / the replay bucket, slice 2.6 Design question 7).
var snapshotWorkspaceWorkflow = fmt.Sprintf(`workflow: conformance-snapshot-ws
version: 1
containers:
  ws:
    image: %[1]s
    snapshot: workspace
  plain:
    image: %[1]s
graph:
  - id: step1
    container: ws
    run: "./step1.sh"
    retry: { attempts: 1 }
  - id: step2
    container: ws
    run: "./step2.sh"
    retry: { attempts: 1 }
    output_files: { state: /work/state.txt }
  - id: step3
    container: plain
    run: "./step3.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// testSnapshot is the Bucket-17 entrypoint. It is FAKE-ONLY: the assertion reads
// the fake's RestoreCalls recorder, which a real (Docker) backend has no
// equivalent for. Skips cleanly on any non-fake backend.
func testSnapshot(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("snapshot bucket asserts on the fake's RestoreCalls recorder; fake-only")
	}
	t.Run("restore_called_on_resume", testSnapshotRestoreCalledOnResume)
}

func testSnapshotRestoreCalledOnResume(t *testing.T) {
	t.Helper()
	h := newSnapshotHarness(t, snapshotWorkspaceWorkflow)
	blobs := h.blobs // the shared store

	// The content step1 writes into ws's fs. The fake serializes ws's whole fs
	// into the snapshot blob at step1's commit, so this is exactly what the
	// snapshot must carry across run→resume.
	snapContent := []byte("workspace state from step1\n")
	const statePath = "/work/state.txt"

	// Each fake shares blobs + is programmed for every step; the run fake crashes
	// step2's exec (FailExecAfterN(1) → the 2nd exec) so step1 fully commits with
	// a snapshot_ref but step2/step3 don't. step1 PRODUCES statePath via the
	// fake's ProgramExecWithFiles affordance — the bytes the snapshot captures.
	// Record run/resume fakes for assertion. NB: besides Restore-was-called, this
	// bucket now asserts the snapshot CONTENT round-trips (step2 captures it off
	// the restored handle) and the non-snapshot container takes the Create path.
	var runFake, resumeFake *container.Fake
	h.factory = func() container.Backend {
		f := container.NewFake().WithBlobs(blobs)
		f.ProgramExecWithFiles("./step1.sh", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{statePath: snapContent})
		f.ProgramExec("./step2.sh", container.ExecResult{ExitCode: 0}, nil)
		f.ProgramExec("./step3.sh", container.ExecResult{ExitCode: 0}, nil)
		if runFake == nil {
			f.FailExecAfterN(1) // crash the 2nd exec (step2) — step1 already committed
			runFake = f
		} else {
			resumeFake = f
		}
		return f
	}

	// First run crashes at step2's exec: step1 commits (with a snapshot_ref),
	// step2 fails the single attempt and propagates a non-ok outcome.
	outcome, _ := h.runWorkflow(t)
	if outcome == "" {
		t.Fatal("first run produced no outcome (harness error before the workflow evaluated)")
	}
	if outcome == engine.OutcomeOK {
		t.Fatal("first run unexpectedly returned ok — FailExecAfterN(1) did not crash step2 (check the retry: {attempts: 1} pin and the fake's one-shot semantic)")
	}

	// step1's node.completed must carry a snapshot_ref.
	var step1Ref string
	for _, e := range mustFoldEvents(t, h) {
		if e.Type == engine.EventNodeCompleted && e.Path == "step1" {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal step1 node.completed: %v", err)
			}
			step1Ref = d.SnapshotRef
		}
	}
	if step1Ref == "" {
		t.Fatal("step1 node.completed has no snapshot_ref — capture didn't happen")
	}

	// Resume: step2/step3 (the frontier) re-execute. ws is RESTORED from step1's
	// ref; step2 runs against the restored handle and its output_files capture
	// reads statePath back off it — which only succeeds if the snapshot's content
	// round-tripped. Resume must therefore complete ok.
	outcome2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if outcome2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok (step2's output_files capture off the restored ws handle must succeed — a missing file would make CaptureFiles error)", outcome2)
	}
	if resumeFake == nil {
		t.Fatal("resume did not mint a second fake")
	}

	// (1) DISPATCH (kept): resume Restored ws from step1's ref (not Created).
	var restoredWS bool
	for _, rc := range resumeFake.RestoreCalls {
		if rc.Name == "ws" && string(rc.Ref) == step1Ref {
			restoredWS = true
		}
	}
	if !restoredWS {
		t.Fatalf("resume did not Restore ws from step1's snapshot (RestoreCalls=%+v, want {ws,%q})", resumeFake.RestoreCalls, step1Ref)
	}

	// (2) CONTENT SURVIVAL: step2 committed with statePath captured off the
	// RESTORED handle; the committed blob's bytes must equal what step1 wrote.
	// This fails if Restore didn't round-trip the snapshot content — it's the
	// proof the snapshot survived resume, not just that Restore was called.
	rs, ferr := engine.Fold(mustFoldEvents(t, h), blobs)
	if ferr != nil {
		t.Fatalf("Fold after resume: %v", ferr)
	}
	step2, ok := rs.Completed["step2"]
	if !ok {
		t.Fatal("step2 not committed after resume — capture off the restored ws handle failed (snapshot content did not survive)")
	}
	stateRef, ok := step2.Files[statePath]
	if !ok {
		t.Fatalf("step2.Files missing %q; got %v", statePath, step2.Files)
	}
	gotBytes, gErr := blobs.Get(stateRef)
	if gErr != nil {
		t.Fatalf("Blobs.Get(%q): %v", stateRef, gErr)
	}
	if string(gotBytes) != string(snapContent) {
		t.Errorf("restored snapshot content = %q, want %q (snapshot did not round-trip through resume)", gotBytes, snapContent)
	}

	// (3) NEGATIVE: `plain` has no snapshot: workspace, so resume Creates it
	// fresh — never Restores it. Lock the mirror-Docker policy (snapshot is
	// opt-in; non-snapshot containers Create fresh).
	for _, rc := range resumeFake.RestoreCalls {
		if rc.Name == "plain" {
			t.Fatalf("resume Restored non-snapshot container %q (RestoreCalls=%+v) — only snapshot: workspace containers may be restored", rc.Name, resumeFake.RestoreCalls)
		}
	}
	var plainCreated bool
	for _, spec := range resumeFake.CreateSpecs {
		if spec.Name == "plain" {
			plainCreated = true
		}
	}
	if !plainCreated {
		t.Fatalf("resume did not Create non-snapshot container plain (CreateSpecs=%+v) — it must take the Create path on resume", resumeFake.CreateSpecs)
	}
}
