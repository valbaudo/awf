package obs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func ev(t *testing.T, typ, path string, ts time.Time, data any) state.Event {
	t.Helper()
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	return state.Event{Type: typ, Path: path, TS: ts, Data: raw}
}

func findSpan(spans []Span, path string) (Span, bool) {
	for _, s := range spans {
		if s.Path == path {
			return s, true
		}
	}
	return Span{}, false
}

func TestProjectStepSpanOK(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	exit := 0
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1", WorkflowID: "wf", WorkflowVersion: 1}),
		ev(t, engine.EventNodeStarted, "s1", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "code"}),
		ev(t, engine.EventNodeCompleted, "s1", t0.Add(3*time.Second), engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit}),
		ev(t, engine.EventRunFinished, "", t0.Add(4*time.Second), engine.RunFinishedData{Outcome: "ok"}),
	}
	spans, err := Project(events, nil)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	s, ok := findSpan(spans, "s1")
	if !ok {
		t.Fatal("no span for s1")
	}
	if s.Pending {
		t.Error("completed step must not be Pending")
	}
	if s.Status != StatusOK {
		t.Errorf("Status = %v, want OK", s.Status)
	}
	if !s.Start.Equal(t0.Add(1*time.Second)) || !s.End.Equal(t0.Add(3*time.Second)) {
		t.Errorf("times = %v..%v, want +1s..+3s", s.Start, s.End)
	}
}

func TestProjectStepSpanFailed(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "s1", t0, engine.NodeStartedData{Kind: "code"}),
		ev(t, engine.EventNodeFailed, "s1", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "permanent_failure", Error: "boom"}),
	}
	spans, _ := Project(events, nil)
	s, _ := findSpan(spans, "s1")
	if s.Status != StatusError {
		t.Errorf("Status = %v, want Error", s.Status)
	}
	if s.Attributes[AttrNodeOutcome] != "permanent_failure" {
		t.Errorf("outcome attr = %v", s.Attributes[AttrNodeOutcome])
	}
}

func TestProjectStepSpanPending(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "s1", t0, engine.NodeStartedData{Kind: "agent"}),
		// no node.completed / node.failed — crashed or in-flight
		ev(t, engine.EventAgentEvent, "s1", t0.Add(5*time.Second), engine.AgentEventData{Kind: "assistant", Size: 3, PayloadInline: []byte("hi")}),
	}
	spans, _ := Project(events, nil)
	s, ok := findSpan(spans, "s1")
	if !ok {
		t.Fatal("no span for s1")
	}
	if !s.Pending {
		t.Fatal("unfinalized step must be Pending")
	}
	if s.Attributes[AttrNodeOutcome] != outcomeIncomplete {
		t.Errorf("Pending outcome attr = %v, want incomplete", s.Attributes[AttrNodeOutcome])
	}
	if !s.End.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("Pending End = %v, want last-event ts +5s", s.End)
	}
	if s.Attributes[AttrNodePath] != "s1" {
		t.Errorf("Pending AttrNodePath = %v, want s1", s.Attributes[AttrNodePath])
	}
	if s.Attributes[AttrNodeKind] != "agent" {
		t.Errorf("Pending AttrNodeKind = %v, want agent", s.Attributes[AttrNodeKind])
	}
}

func TestProjectFailedNoStartedKindUnknown(t *testing.T) {
	// Pre-start failure: only a node.failed, no preceding node.started.
	// obs can't know the kind — AttrNodeKind must be absent, not fabricated.
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeFailed, "s1", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "permanent_failure", Error: "substitution error"}),
	}
	spans, _ := Project(events, nil)
	s, ok := findSpan(spans, "s1")
	if !ok {
		t.Fatal("no span for s1")
	}
	if s.Status != StatusError {
		t.Errorf("Status = %v, want Error", s.Status)
	}
	if s.Kind != "" {
		t.Errorf("Kind = %q, want empty (kind unknown without node.started)", s.Kind)
	}
	if s.Attributes[AttrNodeOutcome] != "permanent_failure" {
		t.Errorf("outcome attr = %v, want permanent_failure", s.Attributes[AttrNodeOutcome])
	}
	if _, present := s.Attributes[AttrNodeKind]; present {
		t.Errorf("AttrNodeKind must be absent for pre-start failure, got %v", s.Attributes[AttrNodeKind])
	}
}

func TestProjectLegacyLogNoNodeStarted(t *testing.T) {
	// Pre-6.1 log: node.completed with no node.started. Project still yields a
	// completion-only span (Start == End == completion ts).
	t0 := time.Unix(1000, 0).UTC()
	exit := 0
	events := []state.Event{
		ev(t, engine.EventNodeCompleted, "s1", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit}),
	}
	spans, _ := Project(events, nil)
	s, ok := findSpan(spans, "s1")
	if !ok {
		t.Fatal("no span for s1 in legacy log")
	}
	if s.Pending {
		t.Error("completed step must not be Pending even without node.started")
	}
	if !s.Start.Equal(t0.Add(2 * time.Second)) {
		t.Errorf("legacy Start = %v, want completion ts (no node.started)", s.Start)
	}
}

func TestProjectSkippedSpan(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeSkipped, "if[0].then.skip[0]", t0, engine.NodeSkippedData{Path: "if[0].then.skip[0]", Reason: "no source"}),
	}
	spans, _ := Project(events, nil)
	s, ok := findSpan(spans, "if[0].then.skip[0]")
	if !ok {
		t.Fatal("no span for skipped node")
	}
	if s.Kind != "skip" || s.Attributes[AttrNodeOutcome] != "skipped" {
		t.Errorf("skip span = kind %q outcome %v, want skip/skipped", s.Kind, s.Attributes[AttrNodeOutcome])
	}
	if s.Attributes["awf.skip.reason"] != "no source" {
		t.Errorf("skip reason = %v", s.Attributes["awf.skip.reason"])
	}
}

func TestProjectResumeDoubleStartLastWins(t *testing.T) {
	// Uncommitted-frontier re-run after a crash: two node.started for one path,
	// then a node.completed. The span reflects the LAST (committed) execution.
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "s1", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}), // epoch-1, abandoned
		ev(t, engine.EventNodeStarted, "s1", t0.Add(5*time.Second), engine.NodeStartedData{Kind: "agent"}), // epoch-2, committed
		ev(t, engine.EventNodeCompleted, "s1", t0.Add(7*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	spans, _ := Project(events, nil)
	s, _ := findSpan(spans, "s1")
	if s.Pending {
		t.Error("a finalized node must not be Pending despite an earlier abandoned start")
	}
	if !s.Start.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("Start = %v, want the LAST node.started (+5s), not the abandoned +1s", s.Start)
	}
}
