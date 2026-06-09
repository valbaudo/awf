package obs

import (
	"reflect"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestProjectIsDeterministic(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1", WorkflowID: "wf", WorkflowVersion: 1}),
		ev(t, engine.EventSkillsSelected, "gate[0].attempt-1.generate.g", t0.Add(500*time.Millisecond), engine.SkillsSelectedData{
			Library:       "stdlib",
			LibraryDigest: "sha256:abc123",
			Router:        "tfidf",
			RouterVersion: "v1.2.3",
			RouterParams: map[string]float64{
				"zeta":  0.7,
				"alpha": 0.1,
			},
			Selected: []engine.SelectedSkill{
				{ID: "skills/search", Score: 0.92},
				{ID: "skills/edit", Score: 0.81},
			},
		}),
		ev(t, engine.EventNodeStarted, "gate[0].attempt-1.generate.g", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "gate[0].attempt-1.generate.g", t0.Add(2*time.Second),
			engine.NodeCompletedData{Outcome: "ok", Metrics: &agent.MetricSet{Cost: agent.MetricCost{USD: 0.03, Source: "reported"}}}),
		ev(t, engine.EventGateAttempt, "gate[0]", t0.Add(3*time.Second), engine.GateAttemptData{N: 1, AttemptOutcome: engine.AttemptPassed}),
		ev(t, engine.EventRunFinished, "", t0.Add(4*time.Second), engine.RunFinishedData{Outcome: "ok"}),
	}
	a, err := Project(events, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Project(events, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Project is not deterministic: two runs over the same log differ")
	}
}
