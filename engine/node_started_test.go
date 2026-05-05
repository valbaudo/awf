package engine

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/state"
)

func TestAppendNodeStartedWritesObservationalEvent(t *testing.T) {
	lg := state.NewInMemoryLog(clock.System{})

	appendNodeStarted(lg, "triage", "agent")

	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != EventNodeStarted {
		t.Errorf("Type = %q, want %q", e.Type, EventNodeStarted)
	}
	if e.Path != "triage" {
		t.Errorf("Path = %q, want %q", e.Path, "triage")
	}
	var d NodeStartedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("unmarshal NodeStartedData: %v", err)
	}
	if d.Kind != "agent" {
		t.Errorf("Kind = %q, want %q", d.Kind, "agent")
	}
}

func TestNodeStartedIgnoredByFold(t *testing.T) {
	// node.started must not affect RunState (observational, Fold default-arm).
	// run.started must be first (Fold's corruption guard); node.started after it
	// must still produce no completed node.
	events := []state.Event{
		{Type: EventRunStarted, Data: mustJSON(RunStartedData{RunID: "r1", WorkflowDigest: "d1"})},
		{Type: EventNodeStarted, Path: "triage", Data: mustJSON(NodeStartedData{Kind: "code"})},
	}
	rs, err := Fold(events, nil)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.LookupCompleted("triage"); ok {
		t.Errorf("node.started must not record a completed node")
	}
}
