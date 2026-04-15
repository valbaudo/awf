package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testReplay is Bucket 2 — exact committed-prefix replay (spec §8 +
// design spec §H). Table-driven over crash points k ∈ [0, 4].
//
// For each k:
//  1. Run a 5-step sequential workflow with FailExecAfterN(k) — step k+1
//     crashes mid-Exec, steps 1..k committed, steps k+2.. unreached.
//  2. Snapshot the post-crash log + the original RunState.Completed[s_i]
//     for i ∈ [1, k].
//  3. ClearFault on log + blobs; resume against the same workflow file.
//  4. Assert: resume's fresh fake (via factory()) NEVER dispatched s_1..s_k
//     (fake.Calls excludes them). RunState.Completed[s_i] for i ∈ [1, k]
//     byte-equals the snapshot. Final outcome ok.
//
// The bucket asserts the invariant for k ∈ {0, 1, 2, 3, 4}; k=0 is the
// edge case "crash on the very first step" (no committed prefix); k=4
// is "crash on the last step" (almost-complete prefix).
func testReplay(t *testing.T, factory BackendFactory) {
	t.Helper()

	stepCommands := []string{"./s1.sh", "./s2.sh", "./s3.sh", "./s4.sh", "./s5.sh"}
	stepIDs := []string{"s1", "s2", "s3", "s4", "s5"}

	for k := 0; k <= 4; k++ {
		k := k // capture
		t.Run("crash-at-step-"+stepIDs[k], func(t *testing.T) {
			programs := make([]execProgram, len(stepCommands))
			for i, cmd := range stepCommands {
				programs[i] = execProgram{
					cmd: cmd,
					res: container.ExecResult{ExitCode: 0},
				}
			}

			// First-run factory: programs all 5 steps, then injects
			// FailExecAfterN(k) so step k+1 crashes.
			firstRunFactory := func() container.Backend {
				b := factory()
				fake, ok := b.(*container.Fake)
				if !ok {
					t.Fatalf("Bucket 2 needs *container.Fake; factory returned %T", b)
				}
				for _, p := range programs {
					fake.ProgramExec(p.cmd, p.res, nil)
				}
				fake.FailExecAfterN(k)
				return fake
			}
			h := newHarness(t, firstRunFactory, fiveStepSeqWorkflow)
			outcome, _ := h.runWorkflow(t)
			// Bucket 2's "crash" must actually halt the step. With the
			// fixture's retry: {attempts: 1} (slice 2.6 Design question
			// 7), the one-shot FailExecAfterN(k) is not recovered; the
			// interpreter classifies the transport error as
			// retryable_failure, exhausts the single attempt, emits
			// node.failed, and propagates. If outcome is ok here, the
			// fault either didn't fire OR the fixture lost its
			// retry-pin — either way the bucket is vacuous and we MUST
			// fail loudly, not silently proceed to the resume.
			if outcome == engine.OutcomeOK {
				t.Fatalf("FailExecAfterN(%d): first run unexpectedly returned ok — the fault did not actually crash step %s (check the fixture's retry: {attempts: 1} pin and the fake's one-shot semantic)", k, stepIDs[k])
			}

			// Snapshot the committed prefix's RunState[i] entries via a
			// FOLD of the current log (the engine wrote them).
			preEvents := mustFoldEvents(t, h)
			preRS, err := engine.Fold(preEvents, h.blobs)
			if err != nil {
				t.Fatalf("Fold pre-resume: %v", err)
			}
			snapshotCompleted := make(map[string]engine.NodeResult, k)
			for i := 0; i < k; i++ {
				nr, ok := preRS.Completed[stepIDs[i]]
				if !ok {
					t.Fatalf("pre-resume RunState.Completed missing %q (expected committed)", stepIDs[i])
				}
				snapshotCompleted[stepIDs[i]] = nr
			}

			// Resume: fresh factory call → fresh fake with no Calls
			// history yet. Program ONLY steps k+1..5 (committed steps
			// must NOT be programmed; if the interpreter dispatches them
			// the fake's ProgramExec-miss error fires and we catch it).
			// The closure captures `resumeFake` so the assertion below
			// can read fake.Calls after h.resumeWorkflow returns.
			var resumeFake *container.Fake
			resumeFactory := func() container.Backend {
				b := factory()
				fake, ok := b.(*container.Fake)
				if !ok {
					t.Fatalf("Bucket 2 needs *container.Fake; factory returned %T", b)
				}
				for i := k; i < len(programs); i++ {
					fake.ProgramExec(programs[i].cmd, programs[i].res, nil)
				}
				resumeFake = fake
				return fake
			}
			// Swap the harness's factory for the resume's. (Otherwise the
			// resume re-runs the first-run factory which still has
			// FailExecAfterN(k) programmed.)
			h.factory = resumeFactory
			// Clear fault hooks on Log + Blobs so the resume's own
			// Appends/Puts don't trip them.
			h.log.ClearFault()
			h.blobs.ClearFault()

			// Resume — engine.Fold builds the RunState from the
			// committed prefix; interpreter skips committed steps; the
			// remaining steps execute.
			resumeOutcome, resumeErr := h.resumeWorkflow(t)
			if resumeErr != nil {
				t.Fatalf("resume: %v", resumeErr)
			}
			if resumeOutcome != engine.OutcomeOK {
				t.Fatalf("resume outcome = %v, want ok", resumeOutcome)
			}

			// All 5 steps committed in the end.
			postEvents := mustFoldEvents(t, h)
			postRS, err := engine.Fold(postEvents, h.blobs)
			if err != nil {
				t.Fatalf("Fold post-resume: %v", err)
			}
			for i := 0; i < 5; i++ {
				if _, ok := postRS.Completed[stepIDs[i]]; !ok {
					t.Errorf("post-resume RunState.Completed missing %q", stepIDs[i])
				}
			}
			// Snapshot equality: steps 1..k have byte-identical NodeResult.
			for i := 0; i < k; i++ {
				orig := snapshotCompleted[stepIDs[i]]
				now := postRS.Completed[stepIDs[i]]
				if !reflect.DeepEqual(orig, now) {
					t.Errorf("step %q NodeResult drifted across resume:\n  pre=%+v\n  post=%+v", stepIDs[i], orig, now)
				}
			}

			// Strongest replay assertion: the resume's fake was NEVER
			// asked to Exec the committed-prefix steps' commands.
			if resumeFake == nil {
				t.Fatal("resumeFactory was never invoked — harness bug")
			}
			for i := 0; i < k; i++ {
				for _, call := range resumeFake.Calls {
					if call.Run == stepCommands[i] {
						t.Errorf("committed step %q was dispatched on resume (call=%+v) — must be replayed from log, not re-executed", stepIDs[i], call)
					}
				}
			}
			// And: post-resume fake DID dispatch the un-committed steps.
			for i := k; i < len(stepCommands); i++ {
				var found bool
				for _, call := range resumeFake.Calls {
					if call.Run == stepCommands[i] {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("un-committed step %q was NOT dispatched on resume; fake.Calls: %+v", stepIDs[i], resumeFake.Calls)
				}
			}
		})
	}
}
