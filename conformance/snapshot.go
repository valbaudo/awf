package conformance

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// snapshotWorkspaceWorkflow — two code steps in a snapshot:workspace container.
// step2's exec is crashed so step1 commits (capturing a snapshot_ref) and the
// frontier (step2) re-executes on resume — at which point resume must RESTORE
// the ws container from step1's snapshot, not Create an empty one.
//
// retry: { attempts: 1 } on both steps pins the one-shot FailExecAfterN fault so
// it actually halts step2 instead of being recovered by the default 3 attempts
// (mirrors fiveStepSeqWorkflow / the replay bucket, slice 2.6 Design question 7).
var snapshotWorkspaceWorkflow = fmt.Sprintf(`workflow: conformance-snapshot-ws
version: 1
containers:
  ws:
    image: %s
    snapshot: workspace
graph:
  - id: step1
    container: ws
    run: "./step1.sh"
    retry: { attempts: 1 }
  - id: step2
    container: ws
    run: "./step2.sh"
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

	// Each fake shares blobs + is programmed for both steps; the run fake crashes
	// step2's exec (FailExecAfterN(1) → the 2nd exec) so step1 fully commits with
	// a snapshot_ref but step2 doesn't. Record run/resume fakes for assertion.
	// NB: this bucket asserts Restore-was-called on resume; it does NOT assert the
	// no-redispatch property (committed step1 not re-executed) — that's the replay
	// bucket's job, which deliberately leaves committed steps unprogrammed.
	var runFake, resumeFake *container.Fake
	h.factory = func() container.Backend {
		f := container.NewFake().WithBlobs(blobs)
		f.ProgramExec("./step1.sh", container.ExecResult{ExitCode: 0}, nil)
		f.ProgramExec("./step2.sh", container.ExecResult{ExitCode: 0}, nil)
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

	// Resume must RESTORE ws from step1's ref (not Create).
	if _, err := h.resumeWorkflow(t); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumeFake == nil {
		t.Fatal("resume did not mint a second fake")
	}
	var restored bool
	for _, rc := range resumeFake.RestoreCalls {
		if rc.Name == "ws" && string(rc.Ref) == step1Ref {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("resume did not Restore ws from step1's snapshot (RestoreCalls=%+v, want {ws,%q})", resumeFake.RestoreCalls, step1Ref)
	}
}
