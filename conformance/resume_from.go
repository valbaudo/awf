package conformance

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testResumeFrom is Bucket resume_from: conformance-level re-fold-determinism
// proof for `awf resume --from <step>`. It verifies that resumeWorkflowFrom
// (a) forces genuine re-execution of the invalidated subtree, and (b) produces
// a log that folds deterministically to the correct committed RunState — the
// delete-arm + last-write-wins compose correctly through a full fold.
func testResumeFrom(t *testing.T, factory BackendFactory) {
	t.Helper()

	// capturingFactory wraps factory so each *container.Fake it returns is
	// programmed with step1+step2 results AND appended to a captured slice.
	// We need the fake references to inspect Calls on the resume epoch's backend.
	var capturedFakes []*container.Fake
	capturingFactory := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./step1.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		fake.ProgramExec("./step2.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		capturedFakes = append(capturedFakes, fake)
		return fake
	}

	h := newHarness(t, capturingFactory, tinySeqWorkflow)

	// 1. First run: both steps must complete ok.
	if outcome, err := h.runWorkflow(t); err != nil || outcome != engine.OutcomeOK {
		t.Fatalf("runWorkflow: outcome=%q err=%v, want OutcomeOK/nil", outcome, err)
	}

	// 2. Sanity: both steps are committed before the resume.
	events1 := mustFoldEvents(t, h)
	rs1, err := engine.Fold(events1, h.blobs)
	if err != nil {
		t.Fatalf("Fold after first run: %v", err)
	}
	if _, ok := rs1.Completed["step1"]; !ok {
		t.Fatalf("pre-resume: step1 not in Completed")
	}
	if _, ok := rs1.Completed["step2"]; !ok {
		t.Fatalf("pre-resume: step2 not in Completed")
	}

	// 3. Resume --from step1: invalidates step1 and step2 (step1's subtree
	// includes everything that follows in the sequential graph).
	if outcome, err := h.resumeWorkflowFrom(t, "step1"); err != nil || outcome != engine.OutcomeOK {
		t.Fatalf("resumeWorkflowFrom(step1): outcome=%q err=%v, want OutcomeOK/nil", outcome, err)
	}

	// 4. Re-run observed: the resume epoch's fake (index 1, the second captured)
	// must have dispatched BOTH ./step1.sh and ./step2.sh — proving invalidation
	// forced genuine re-execution rather than a no-op replay.
	if len(capturedFakes) < 2 {
		t.Fatalf("expected at least 2 captured fakes (run + resume), got %d", len(capturedFakes))
	}
	resumeFake := capturedFakes[len(capturedFakes)-1]
	sawStep1, sawStep2 := false, false
	for _, c := range resumeFake.Calls {
		if c.Run == "./step1.sh" {
			sawStep1 = true
		}
		if c.Run == "./step2.sh" {
			sawStep2 = true
		}
	}
	if !sawStep1 {
		t.Errorf("resume epoch fake: ./step1.sh not in Calls — step1 was NOT re-executed (invalidation failed)")
	}
	if !sawStep2 {
		t.Errorf("resume epoch fake: ./step2.sh not in Calls — step2 was NOT re-executed (invalidation failed)")
	}

	// 5. Re-fold determinism: fold the final log from scratch and assert both
	// steps survive invalidate + re-commit. The delete-arm + last-write-wins
	// must compose correctly through a full fold.
	events2 := mustFoldEvents(t, h)
	rs2, err := engine.Fold(events2, h.blobs)
	if err != nil {
		t.Fatalf("Fold after resume: %v", err)
	}
	if _, ok := rs2.Completed["step1"]; !ok {
		t.Errorf("re-fold: step1 missing from Completed after --from resume")
	}
	if _, ok := rs2.Completed["step2"]; !ok {
		t.Errorf("re-fold: step2 missing from Completed after --from resume")
	}

	// 6. Engine appended exactly one node.invalidated event, and its Paths
	// equals the sorted set ["step1","step2"] (ComputeRerunInvalidation returns
	// sorted; --from step1 invalidates step1 ∪ all successors = {step1, step2}).
	invalidatedCount := 0
	var invalidatedPaths []string
	for _, e := range events2 {
		if e.Type != engine.EventNodeInvalidated {
			continue
		}
		invalidatedCount++
		var d engine.NodeInvalidatedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal node.invalidated: %v", err)
		}
		invalidatedPaths = d.Paths
	}
	if invalidatedCount != 1 {
		t.Errorf("node.invalidated count = %d, want 1", invalidatedCount)
	}
	wantPaths := []string{"step1", "step2"}
	if !reflect.DeepEqual(invalidatedPaths, wantPaths) {
		t.Errorf("node.invalidated Paths = %v, want %v", invalidatedPaths, wantPaths)
	}
}
