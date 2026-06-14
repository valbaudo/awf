package engine

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/state"
)

func TestRunFinishedDataFromEvent(t *testing.T) {
	data, err := json.Marshal(RunFinishedData{Outcome: string(OutcomeRetryableFailure)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: data})
	if err != nil {
		t.Fatalf("RunFinishedDataFromEvent: %v", err)
	}
	if d.Outcome != string(OutcomeRetryableFailure) {
		t.Errorf("Outcome = %q, want %q", d.Outcome, OutcomeRetryableFailure)
	}
}

func TestNodeFailedDataFromEvent(t *testing.T) {
	data, err := json.Marshal(NodeFailedData{Outcome: string(OutcomePermanentFailure), Error: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := NodeFailedDataFromEvent(state.Event{Type: EventNodeFailed, Path: "map[0].item-2", Data: data})
	if err != nil {
		t.Fatalf("NodeFailedDataFromEvent: %v", err)
	}
	if d.Outcome != string(OutcomePermanentFailure) {
		t.Errorf("Outcome = %q, want %q", d.Outcome, OutcomePermanentFailure)
	}
}

func TestRunFinishedDataFromEventBadJSON(t *testing.T) {
	if _, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: []byte("{not json")}); err == nil {
		t.Error("want error on malformed payload, got nil")
	}
}
