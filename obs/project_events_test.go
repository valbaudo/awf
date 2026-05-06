package obs

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestProjectGateAttemptAttrsAndEvalEvent(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "gate[0].attempt-1.generate.exploit", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "gate[0].attempt-1.generate.exploit", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
		ev(t, engine.EventGateAttempt, "gate[0]", t0.Add(3*time.Second), engine.GateAttemptData{N: 1, AttemptOutcome: engine.AttemptRejected}),
		ev(t, engine.EventNodeStarted, "gate[0].attempt-2.generate.exploit", t0.Add(4*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "gate[0].attempt-2.generate.exploit", t0.Add(6*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
		ev(t, engine.EventGateAttempt, "gate[0]", t0.Add(7*time.Second), engine.GateAttemptData{N: 2, AttemptOutcome: engine.AttemptPassed}),
	}
	spans, _ := Project(events, nil)
	gate, ok := findSpan(spans, "gate[0]")
	if !ok {
		t.Fatal("no gate span")
	}
	if gate.Attributes[AttrGateAttempts] != int64(2) {
		t.Errorf("gate attempts = %v, want 2", gate.Attributes[AttrGateAttempts])
	}
	if gate.Attributes[AttrGateOutcome] != "passed" {
		t.Errorf("gate outcome = %v, want passed", gate.Attributes[AttrGateOutcome])
	}
	// Two gen_ai.evaluation.result events parented to the gate span.
	if len(gate.Events) != 2 {
		t.Fatalf("got %d eval events, want 2", len(gate.Events))
	}
	if gate.Events[0].Name != EventGenAIEvaluation {
		t.Errorf("event name = %q, want %q", gate.Events[0].Name, EventGenAIEvaluation)
	}
}

func TestProjectBranchTakenAttr(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventBranchTaken, "if[0]", t0, engine.BranchTakenData{Which: "then"}),
		ev(t, engine.EventNodeStarted, "if[0].then.s", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "code"}),
		ev(t, engine.EventNodeCompleted, "if[0].then.s", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	spans, _ := Project(events, nil)
	ifs, _ := findSpan(spans, "if[0]")
	if ifs.Attributes["awf.branch"] != "then" {
		t.Errorf("branch attr = %v, want then", ifs.Attributes["awf.branch"])
	}
}
