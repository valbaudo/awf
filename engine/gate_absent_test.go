package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
)

// TestRunGateAbsentIfRefDoesNotConsumeAttempt pins the crash≠verdict invariant
// for AWF4006 ABSENT: when a gate's generate step references a step that sits
// under a NON-taken if branch, template substitution fails with AWF4006
// (permanent_failure). The gate executor propagates this mechanical failure BEFORE
// writing a gate.attempt event — no attempt is consumed.
//
// Wiring: the workflow has if[0] (branch "else" pre-recorded) whose then-branch
// contains step "deep". Gate gate[1]'s generate step runs
// "echo {{ step.deep.summary }}", which resolves to AWF4006 because
// rs.Branches["if[0]"] = "else" → absentDueToUntakenIf returns true for
// "if[0].then.deep". The AWF4006 propagates as permanent_failure from generate
// BEFORE the gate.attempt event is written.
//
// Gate-attempt accessor used: rs.LookupGateAttempts("gate[1]") — defined in
// engine/runstate.go:471.
func TestRunGateAbsentIfRefDoesNotConsumeAttempt(t *testing.T) {
	g := &ir.Gate{
		Generate: ir.NodeList{
			// run: references a step under a non-taken if branch → AWF4006 at
			// template substitution, before the dispatcher is ever reached.
			&ir.CodeStep{ID: "gen1", Run: "echo {{ step.deep.summary }}", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       ir.Expr("{{ evaluate.verified }}"),
		MaxAttempts: 3,
	}
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					// "deep" sits under if[0].then; its static path is "if[0].then.deep".
					&ir.CodeStep{
						ID:           "deep",
						Container:    "c0",
						Run:          "./deep.sh",
						OutputSchema: awfStringObjectSchema("summary"),
					},
				},
			},
			g, // gate[1]
		},
	}

	// Empty script: if gen1 or eval1 ever reach the dispatcher they call t.Fatalf
	// — template substitution must fail before dispatch.
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{})

	rs := NewRunState("run-x", "digest", nil)
	// Pre-seed the branch decision: if[0] took "else" (then-branch NOT taken).
	// On a real resume this comes from Fold populating rs.Branches from a
	// committed branch.taken event; here we model that committed state directly.
	rs.RecordBranch("if[0]", "else")

	oc, err := runGate(context.Background(), g, "gate[1]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)

	// AWF4006 in generate is a mechanical failure → permanent_failure propagates.
	if oc != OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure (AWF4006 from generate's absent ref)", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4006") {
		t.Errorf("err = %v, want AWF4006 in message", err)
	}

	// Crash≠verdict: the gate.attempt event must NOT have been committed.
	if got := rs.LookupGateAttempts("gate[1]"); len(got) != 0 {
		t.Errorf("GateAttempts len = %d, want 0 (crash≠verdict — AWF4006 in generate must not consume an attempt)", len(got))
	}
	// Confirm by scanning the log (InMemoryLog.Fold returns all appended events).
	events, _ := lg.Fold()
	for _, e := range events {
		if e.Type == EventGateAttempt {
			t.Errorf("unexpected gate.attempt event in log: %+v", e)
		}
	}
}

// TestRunGateAbsentIfRefInUntilDoesNotConsumeAttempt is the STRONG crash≠verdict
// pin: AWF4006 at the UNTIL position (gate.go:141-146), where a real `false`
// until WOULD fall through to step 5 (RecordGateAttempt). This proves the until
// expression was reached (generate + evaluate both succeeded, a verdict was
// looked up) but the mechanical AWF4006 error branched off to permanent_failure
// BEFORE consuming an attempt — the genuine distinction the generate-position
// test above cannot make (generate returns before any attempt could be recorded).
//
// Wiring: if[0] pre-recorded as "else" (then-branch NOT taken) so step "deep" is
// absent. Generate scripts OK, evaluate scripts a valid verdict, and
// g.Until = "{{ step.deep.summary }}" references the absent under-if step. The
// until evaluation (template.EvalBoolString) resolves step.deep → AWF4006.
func TestRunGateAbsentIfRefInUntilDoesNotConsumeAttempt(t *testing.T) {
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		// until references a step under a non-taken if branch → AWF4006 at the
		// until position, AFTER generate + evaluate succeed and the verdict is read.
		Until:       ir.Expr("{{ step.deep.summary }}"),
		MaxAttempts: 3,
	}
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{
						ID:           "deep",
						Container:    "c0",
						Run:          "./deep.sh",
						OutputSchema: awfStringObjectSchema("summary"),
					},
				},
			},
			g, // gate[1]
		},
	}

	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "ok"}},
	})

	rs := NewRunState("run-x", "digest", nil)
	rs.RecordBranch("if[0]", "else") // then-branch NOT taken → "deep" is absent

	oc, err := runGate(context.Background(), g, "gate[1]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)

	// AWF4006 in until is a mechanical template error → permanent_failure per
	// gate.go's "author's bug" classification (same as runIf / runLoop DQ7).
	if oc != OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure (AWF4006 from until's absent ref)", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4006") {
		t.Errorf("err = %v, want AWF4006 in message", err)
	}

	// The genuine crash≠verdict pin: the until WAS reached (generate + evaluate
	// succeeded), but the mechanical error branched off BEFORE step 6
	// (RecordGateAttempt) — no attempt consumed.
	if got := rs.LookupGateAttempts("gate[1]"); len(got) != 0 {
		t.Errorf("GateAttempts len = %d, want 0 (crash≠verdict — AWF4006 in until must not consume an attempt)", len(got))
	}
	events, _ := lg.Fold()
	for _, e := range events {
		if e.Type == EventGateAttempt {
			t.Errorf("unexpected gate.attempt event in log: %+v", e)
		}
	}
}
