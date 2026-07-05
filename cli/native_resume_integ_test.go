//go:build integ

package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// TestCLINativeRunPauseResumeRestoresWorkspaceSnapshot is the content-survival
// proof for native resume — the assertion the fake conformance suite does NOT
// make. It drives a real `awf run --backend native` (host processes via sh -c,
// NO Docker, NO live API) of a `snapshot: workspace` workflow:
//
//  1. step_a writes a unique sentinel into the container workspace (marker.txt)
//     and commits. Because the container is snapshot:workspace, the dispatcher
//     captures the CoW diff into Blobs and records its ref on step_a's
//     node.completed (engine/local_dispatcher.go:294-305).
//  2. wait_for_resume (await:) blocks in the broker; pause fires while parked
//     there (the same deterministic suspend point cloned from the Docker
//     pause-resume round-trip, TestCLIRunDockerBackendPauseResumeRoundTrip in
//     cli/run_backend_integ_test.go). On ErrPaused the run halts clean with
//     skipTeardown=true, so the deferred Destroy does NOT fire and the workdir
//     survives.
//  3. We then DELETE the native workdir (<stateDir>/work/ws). This is what
//     makes the test meaningful: if Restore did not actually re-materialize the
//     snapshot, step_b's `cat marker.txt` would fail and the sentinel would not
//     appear in step_b's typed output.
//  4. `awf resume` reconstructs the native backend, folds SnapshotRefs from the
//     log, and Restores ws from the captured diff BEFORE running the frontier
//     (engine/runtime_handles.go:42-45). step_b reads marker.txt and emits its
//     contents as typed output.
//
// Assertions: resume exits OK; step_b's typed output carries the sentinel
// (so the snapshot content survived, not a silently-empty workdir); and the
// re-folded log's SnapshotRefs[ws] is non-empty (the diff was captured).
//
// Suspend mechanism: the await: + cli "pause"/"signal" subcommands, identical
// to the Docker round-trip test. No new mechanism is invented.
func TestCLINativeRunPauseResumeRestoresWorkspaceSnapshot(t *testing.T) {
	tmp := t.TempDir()
	stateDir := t.TempDir()
	sentinel := fmt.Sprintf("native-snapshot-sentinel-%d", time.Now().UnixNano())

	// Native ignores spec.Image (it runs on the host), but snapshot:workspace
	// is only valid on an image-mode container (AWF1022 rejects compose), and
	// the IR validator requires the oci://...@sha256 digest-pinned form. The
	// all-zeros digest is never pulled by native — it's purely structural.
	//
	// step_a writes the sentinel into the workspace (cwd = the native workdir).
	// step_b mkdir's $AWF_OUTPUT's dir (host path /tmp/awf per spec §4.1) and
	// emits the marker's content as typed output so we can assert it survived.
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: native-snapshot-resume
version: 1
containers:
  ws:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
    snapshot: workspace
graph:
  - id: step_a
    container: ws
    run: "echo `+sentinel+` > marker.txt"
  - id: wait_for_resume
    await: resume_marker
  - id: step_b
    container: ws
    run: 'mkdir -p "$(dirname "$AWF_OUTPUT")"; printf ''{"content":"%s"}'' "$(cat marker.txt)" > "$AWF_OUTPUT"'
    output_schema:
      type: object
      additionalProperties: false
      required: [content]
      properties:
        content: { type: string }
`), 0o600); err != nil {
		t.Fatal(err)
	}

	runID := fmt.Sprintf("native-snapshot-resume-%d", time.Now().UnixNano())

	// First invocation in a goroutine: step_a commits (snapshot captured), then
	// the engine blocks in await(resume_marker). pause.json unblocks the await
	// with ErrPaused → cli/execute.go halts clean (rc=ExitOK, skipTeardown=true).
	runner1 := &cli.Runner{IDGen: &clock.Fake{IDs: []string{runID}}}
	done := make(chan runResult, 1)
	go func() {
		var sout, serr bytes.Buffer
		rc := runner1.Run(
			[]string{"run", "--state-dir", stateDir, "--run-id", runID, "--backend", "native", wfPath},
			&sout, &serr,
		)
		done <- runResult{rc: rc, stderr: serr.String()}
	}()

	// Wait for step_a's commit, then the engine is at (or entering) the await.
	waitForCommittedStep(t, stateDir, runID, "step_a")

	pauseRunner := &cli.Runner{IDGen: &clock.Fake{}}
	var pstdout, pstderr bytes.Buffer
	prc := pauseRunner.Run(
		[]string{"pause", "--state-dir", stateDir, runID, "--reason", "native-snapshot resume integ"},
		&pstdout, &pstderr,
	)
	if prc != cli.ExitOK {
		t.Fatalf("pause rc = %d; stderr: %s", prc, pstderr.String())
	}

	select {
	case result := <-done:
		if result.rc != cli.ExitOK {
			t.Fatalf("first run rc = %d, want ExitOK (pause should be clean); stderr: %s", result.rc, result.stderr)
		}
	case <-time.After(runReturnAfterPauseLimit):
		t.Fatal("first run did not return within timeout after pause")
	}

	// Sanity: step_a's snapshot ref landed in the log; step_b did NOT run yet.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	{
		fl, err := state.OpenLog(logPath, clock.System{})
		if err != nil {
			t.Fatalf("OpenLog: %v", err)
		}
		events, err := fl.Fold()
		_ = fl.Close()
		if err != nil {
			t.Fatalf("Fold: %v", err)
		}
		var startedBackend, stepASnapshotRef string
		var sawPaused, sawStepB bool
		for _, e := range events {
			switch e.Type {
			case engine.EventRunStarted:
				var d engine.RunStartedData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatalf("Unmarshal run.started: %v", err)
				}
				startedBackend = d.Backend
			case engine.EventNodeCompleted:
				if e.Path == "step_a" {
					var d engine.NodeCompletedData
					if err := json.Unmarshal(e.Data, &d); err != nil {
						t.Fatalf("Unmarshal step_a node.completed: %v", err)
					}
					stepASnapshotRef = d.SnapshotRef
				}
				if e.Path == "step_b" {
					sawStepB = true
				}
			case engine.EventRunPaused:
				sawPaused = true
			}
		}
		if startedBackend != engine.BackendNative {
			t.Errorf("run.started.Backend = %q, want %q", startedBackend, engine.BackendNative)
		}
		if !sawPaused {
			t.Error("missing run.paused after first run")
		}
		if stepASnapshotRef == "" {
			t.Fatal("step_a node.completed has empty snapshot_ref — snapshot:workspace did not capture")
		}
		if sawStepB {
			t.Error("step_b committed before resume — the await did not actually block")
		}
	}

	// DELETE the native workdir BEFORE resume. native Create/Restore use
	// filepath.Join(workdirRoot, name); U3/F26 made workdirRoot per-run
	// (<stateDir>/work/<run-id>) — run.go and resume.go both compute
	// filepath.Join(stateDir, "work", runID) — and name is the (top-level,
	// unqualified) container name "ws". After this, Restore is the SOLE
	// source of marker.txt — if it no-ops, step_b's cat fails.
	workdir := filepath.Join(stateDir, "work", runID, "ws")
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("native workdir %q not present before delete (path layout wrong?): %v", workdir, err)
	}
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatalf("RemoveAll(%q): %v", workdir, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("marker.txt still present after workdir delete (err=%v) — delete did not take", err)
	}

	// Pre-load the signal that unblocks the await on resume (buffered; consumed
	// when the await is reached). No --backend flag: resume reads native from
	// run.started.
	sigRunner := &cli.Runner{IDGen: &clock.Fake{}}
	var sstdout, sstderr bytes.Buffer
	src := sigRunner.Run(
		[]string{"signal", "--state-dir", stateDir, runID, "resume_marker"},
		&sstdout, &sstderr,
	)
	if src != cli.ExitOK {
		t.Fatalf("signal rc = %d; stderr: %s", src, sstderr.String())
	}

	runner2 := &cli.Runner{IDGen: &clock.Fake{}}
	resumeDone := make(chan runResult, 1)
	go func() {
		var sout, serr bytes.Buffer
		rc := runner2.Run(
			[]string{"resume", "--state-dir", stateDir, runID, wfPath},
			&sout, &serr,
		)
		resumeDone <- runResult{rc: rc, stderr: serr.String()}
	}()
	var resume runResult
	select {
	case resume = <-resumeDone:
		if resume.rc != cli.ExitOK {
			t.Fatalf("resume rc = %d, want ExitOK; stderr: %s", resume.rc, resume.stderr)
		}
	case <-time.After(resumeCompletionLimit):
		t.Fatal("resume did not complete within timeout")
	}

	// Final assertions: step_b committed with the sentinel in its typed output
	// (snapshot content survived), and SnapshotRefs[ws] is non-empty.
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog (post-resume): %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold (post-resume): %v", err)
	}

	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}

	rs, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	if rs.SnapshotRefs["ws"] == "" {
		t.Errorf("SnapshotRefs[ws] empty after resume — no captured snapshot for the container")
	}
	var stepBOutputsRef string
	var sawStepB, sawFinished bool
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeCompleted:
			if e.Path == "step_b" {
				sawStepB = true
				var d engine.NodeCompletedData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatalf("Unmarshal step_b node.completed: %v", err)
				}
				stepBOutputsRef = d.OutputsRef
			}
		case engine.EventRunFinished:
			sawFinished = true
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("Unmarshal run.finished: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if !sawStepB {
		t.Fatal("missing node.completed at path step_b after resume — frontier did not advance")
	}
	if !sawFinished || finishedOutcome != "ok" {
		t.Errorf("run.finished outcome = %q (saw=%v), want \"ok\"", finishedOutcome, sawFinished)
	}
	if stepBOutputsRef == "" {
		t.Fatal("step_b node.completed has empty outputs_ref — typed output was not captured")
	}
	out, err := blobs.Get(stepBOutputsRef)
	if err != nil {
		t.Fatalf("Get step_b outputs blob: %v", err)
	}
	var typed struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &typed); err != nil {
		t.Fatalf("Unmarshal step_b typed output %q: %v", string(out), err)
	}
	if !strings.Contains(typed.Content, sentinel) {
		t.Fatalf("step_b typed output content = %q, want it to contain sentinel %q — the restored workspace did not carry marker.txt", typed.Content, sentinel)
	}
}
