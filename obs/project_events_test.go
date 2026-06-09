package obs

import (
	"strings"
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

func TestProjectSkillsSelectedEvent(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	querySentinel := "find the moonlit refactor"
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1"}),
		ev(t, engine.EventSkillsSelected, "hunt", t0.Add(1*time.Second), engine.SkillsSelectedData{
			Library:       "stdlib",
			LibraryDigest: "sha256:abc123",
			Router:        "tfidf",
			RouterVersion: "v1.2.3",
			RouterParams: map[string]float64{
				"alpha": 0.25,
				"beta":  2,
			},
			Selected: []engine.SelectedSkill{
				{ID: "skills/search", Score: 0.92},
				{ID: "skills/edit", Score: 0.81},
			},
		}),
		ev(t, engine.EventNodeStarted, "hunt", t0.Add(2*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "hunt", t0.Add(3*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}

	spans, err := Project(events, nil)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	hunt, ok := findSpan(spans, "hunt")
	if !ok {
		t.Fatal("no hunt span")
	}
	if len(hunt.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(hunt.Events))
	}
	got := hunt.Events[0]
	if got.Name != "awf.skills.selected" {
		t.Fatalf("event name = %q, want awf.skills.selected", got.Name)
	}

	wantAttrs := map[string]any{
		"awf.skills.library":            "stdlib",
		"awf.skills.library_digest":     "sha256:abc123",
		"awf.skills.router":             "tfidf",
		"awf.skills.router_version":     "v1.2.3",
		"awf.skills.selected.count":     int64(2),
		"awf.skills.selected.0.id":      "skills/search",
		"awf.skills.selected.0.score":   0.92,
		"awf.skills.selected.1.id":      "skills/edit",
		"awf.skills.selected.1.score":   0.81,
		"awf.skills.router_param.alpha": 0.25,
		"awf.skills.router_param.beta":  2.0,
	}
	for k, want := range wantAttrs {
		if got.Attributes[k] != want {
			t.Errorf("attr %s = %v, want %v", k, got.Attributes[k], want)
		}
	}
	for k, v := range got.Attributes {
		if strings.Contains(k, "query") {
			t.Fatalf("unexpected query-related attr key %q", k)
		}
		if s, ok := v.(string); ok && (strings.Contains(s, "query") || strings.Contains(s, querySentinel)) {
			t.Fatalf("unexpected query-related attr value %q for key %q", s, k)
		}
	}
}

func TestProjectSkillsSelectedPendingSpanStartsAtEventTime(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	skillsTS := t0.Add(2 * time.Second)
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1"}),
		ev(t, engine.EventSkillsSelected, "hunt", skillsTS, engine.SkillsSelectedData{
			Library:       "stdlib",
			LibraryDigest: "sha256:abc123",
			Router:        "tfidf",
			RouterVersion: "v1",
			Selected:      []engine.SelectedSkill{{ID: "skills/search", Score: 0.92}},
		}),
	}

	spans, err := Project(events, nil)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	hunt, ok := findSpan(spans, "hunt")
	if !ok {
		t.Fatal("no hunt span")
	}
	if !hunt.Pending {
		t.Fatal("hunt span must be pending")
	}
	if !hunt.Start.Equal(skillsTS) {
		t.Fatalf("hunt start = %v, want skills.selected timestamp %v", hunt.Start, skillsTS)
	}
	if !hunt.End.Equal(skillsTS) {
		t.Fatalf("hunt end = %v, want last observed timestamp %v", hunt.End, skillsTS)
	}
	if len(hunt.Events) != 1 || hunt.Events[0].Name != EventNameSkillsSelected {
		t.Fatalf("hunt events = %#v, want one %s event", hunt.Events, EventNameSkillsSelected)
	}
}
