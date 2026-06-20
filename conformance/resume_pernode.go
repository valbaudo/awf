package conformance

import (
	"fmt"
	"os"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// perNodeWorkflow builds a two-code-step sequential workflow (a → b) using the
// provided run bodies. Changing runB's body (but not runA's) changes only the
// NodeSubtreeDigest for "b", so verifying-trace can reuse "a" and only re-run "b".
func perNodeWorkflow(runA, runB string) string {
	return fmt.Sprintf(`workflow: conformance-pernode-ab
version: 1
containers:
  lab:
    image: %s
graph:
  - id: a
    container: lab
    run: "%s"
  - id: b
    container: lab
    run: "%s"
`, fakeImageDigest, runA, runB)
}

// testResumePerNode is Bucket resume_pernode (WS-6b): conformance-level proof for
// the per-node verifying-trace resume path. Three sub-tests:
//
//   - edit_downstream: only step b's body changes → a is reused, b re-runs.
//   - edit_upstream: only step a's body changes → both a and b re-run.
//   - agent_not_reusable: an AgentStep commit has NodeSubtreeDigest="" and is always
//     treated as non-reusable by ComputeVerifyingTraceTarget. Tested via direct
//     engine.ComputeVerifyingTraceTarget assertion over a synthesized RunState (no
//     dispatch: the dispatch behavior is identical to edit_upstream, and agent_step
//     conformance already covers agent execution).
func testResumePerNode(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("edit_downstream", func(t *testing.T) { testResumePerNodeEditDownstream(t, factory) })
	t.Run("edit_upstream", func(t *testing.T) { testResumePerNodeEditUpstream(t, factory) })
	t.Run("agent_not_reusable", func(t *testing.T) { testResumePerNodeAgentNotReusable(t) })
}

func testResumePerNodeEditDownstream(t *testing.T, factory BackendFactory) {
	t.Helper()

	origRunA := "./a.sh"
	origRunB := "./b.sh"
	changedRunB := "./b-CHANGED.sh"

	// capturingFactory wraps factory so each *container.Fake is pre-programmed and
	// captured. We inspect the resume epoch's Calls to assert which scripts ran.
	var capturedFakes []*container.Fake
	capturingFactory := func() container.Backend {
		b := factory()
		fk, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fk.ProgramExec(origRunA, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		fk.ProgramExec(origRunB, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		fk.ProgramExec(changedRunB, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		capturedFakes = append(capturedFakes, fk)
		return fk
	}

	h := newHarness(t, capturingFactory, perNodeWorkflow(origRunA, origRunB))
	h.recordStructuralDigest = true

	// First run: both a and b complete ok.
	if oc, err := h.runWorkflow(t); err != nil || oc != engine.OutcomeOK {
		t.Fatalf("first run: outcome=%q err=%v, want OutcomeOK/nil", oc, err)
	}

	// Edit b's body only — write the updated workflow to the harness path.
	updatedYAML := perNodeWorkflow(origRunA, changedRunB)
	if err := os.WriteFile(h.wfPath, []byte(updatedYAML), 0o644); err != nil {
		t.Fatalf("write updated workflow: %v", err)
	}

	// Verifying-trace resume: structural guard passes (only body changed), target=b.
	if oc, err := h.resumeWorkflowVerifyingTrace(t); err != nil || oc != engine.OutcomeOK {
		t.Fatalf("verifying-trace resume: outcome=%q err=%v, want OutcomeOK/nil", oc, err)
	}

	// Assert dispatch: resume epoch's fake must have dispatched b-CHANGED but NOT a.
	if len(capturedFakes) < 2 {
		t.Fatalf("expected ≥2 captured fakes (run + resume), got %d", len(capturedFakes))
	}
	resumeFake := capturedFakes[len(capturedFakes)-1]
	sawA, sawB := false, false
	for _, c := range resumeFake.Calls {
		if c.Run == origRunA {
			sawA = true
		}
		if c.Run == changedRunB {
			sawB = true
		}
	}
	if sawA {
		t.Errorf("resume epoch: %q was dispatched — expected reuse (a unchanged)", origRunA)
	}
	if !sawB {
		t.Errorf("resume epoch: %q was NOT dispatched — expected re-execution (b changed)", changedRunB)
	}
}

func testResumePerNodeEditUpstream(t *testing.T, factory BackendFactory) {
	t.Helper()

	origRunA := "./a.sh"
	origRunB := "./b.sh"
	changedRunA := "./a-CHANGED.sh"

	var capturedFakes []*container.Fake
	capturingFactory := func() container.Backend {
		b := factory()
		fk, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fk.ProgramExec(origRunA, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		fk.ProgramExec(origRunB, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		fk.ProgramExec(changedRunA, container.ExecResult{ExitCode: 0, AWFOutput: []byte("{}")}, nil)
		capturedFakes = append(capturedFakes, fk)
		return fk
	}

	h := newHarness(t, capturingFactory, perNodeWorkflow(origRunA, origRunB))
	h.recordStructuralDigest = true

	// First run: both steps complete ok.
	if oc, err := h.runWorkflow(t); err != nil || oc != engine.OutcomeOK {
		t.Fatalf("first run: outcome=%q err=%v, want OutcomeOK/nil", oc, err)
	}

	// Edit only a's body.
	updatedYAML := perNodeWorkflow(changedRunA, origRunB)
	if err := os.WriteFile(h.wfPath, []byte(updatedYAML), 0o644); err != nil {
		t.Fatalf("write updated workflow: %v", err)
	}

	// Verifying-trace resume: target=a (earliest slot changed), so both a and b re-run.
	if oc, err := h.resumeWorkflowVerifyingTrace(t); err != nil || oc != engine.OutcomeOK {
		t.Fatalf("verifying-trace resume: outcome=%q err=%v, want OutcomeOK/nil", oc, err)
	}

	if len(capturedFakes) < 2 {
		t.Fatalf("expected ≥2 captured fakes (run + resume), got %d", len(capturedFakes))
	}
	resumeFake := capturedFakes[len(capturedFakes)-1]
	sawA, sawB := false, false
	for _, c := range resumeFake.Calls {
		if c.Run == changedRunA {
			sawA = true
		}
		if c.Run == origRunB {
			sawB = true
		}
	}
	if !sawA {
		t.Errorf("resume epoch: %q was NOT dispatched — expected re-execution (a changed)", changedRunA)
	}
	if !sawB {
		t.Errorf("resume epoch: %q was NOT dispatched — expected re-execution (b after changed a)", origRunB)
	}
}

// testResumePerNodeAgentNotReusable asserts, via engine.ComputeVerifyingTraceTarget
// over a synthesized RunState, that an AgentStep commit is always non-reusable.
// engine.Commit stores NodeSubtreeDigest="" for agent steps (only code steps get a
// non-empty value); ComputeVerifyingTraceTarget's reusable() predicate requires
// NodeSubtreeDigest != "" and the node to be *ir.CodeStep — so agent steps are
// unconditionally non-reusable.
//
// This sub-test does NOT dispatch any containers. The dispatch behavior (all steps
// after the agent re-run) is identical to edit_upstream and is covered by the
// engine unit test TestComputeVerifyingTraceTarget_AgentNotReusable.
func testResumePerNodeAgentNotReusable(t *testing.T) {
	t.Helper()

	// Build a two-node workflow: AgentStep "triage" followed by CodeStep "report".
	wf := &ir.Workflow{
		ID:      "pernode-agent-test",
		Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "triage", Uses: "awf/llm"},
			&ir.CodeStep{ID: "report", Container: "lab", Run: "./report.sh"},
		},
	}

	// Compute the subtree digest for "report" so it matches the committed record.
	reportDigest, err := ir.NodeSubtreeDigest(&ir.CodeStep{ID: "report", Container: "lab", Run: "./report.sh"})
	if err != nil {
		t.Fatalf("NodeSubtreeDigest(report): %v", err)
	}

	// Synthesize a RunState where both steps are committed. The agent step has
	// NodeSubtreeDigest="" (as engine.Commit produces for non-code steps).
	rs := &engine.RunState{
		Completed: map[string]engine.NodeResult{
			"triage": {Outcome: engine.OutcomeOK, NodeSubtreeDigest: ""},
			"report": {Outcome: engine.OutcomeOK, NodeSubtreeDigest: reportDigest},
		},
	}

	// ComputeVerifyingTraceTarget must return "triage" (slot 0, non-reusable).
	target, err := engine.ComputeVerifyingTraceTarget(wf, rs)
	if err != nil {
		t.Fatalf("ComputeVerifyingTraceTarget: unexpected error: %v", err)
	}
	if target != "triage" {
		t.Errorf("target = %q, want %q (agent always non-reusable → earliest slot)", target, "triage")
	}
}
