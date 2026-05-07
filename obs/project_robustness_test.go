package obs

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// TestProjectMalformedPayloadReturnsError — Regression guarded: a corrupt event
// payload must surface as an error, never a panic (obs is a damaged-run reader).
func TestProjectMalformedPayloadReturnsError(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	rs := ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"})
	cases := []struct {
		name string
		bad  state.Event
	}{
		{"gate.attempt", state.Event{Type: engine.EventGateAttempt, Path: "gate[0]", TS: t0, Data: []byte("{")}},
		{"node.started", state.Event{Type: engine.EventNodeStarted, Path: "s1", TS: t0, Data: []byte("{not json")}},
		{"node.completed", state.Event{Type: engine.EventNodeCompleted, Path: "s1", TS: t0, Data: []byte("nope")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Project([]state.Event{rs, c.bad}, nil); err == nil {
				t.Fatalf("Project: want error on malformed %s payload, got nil", c.name)
			}
		})
	}
}

// TestProjectEmptyAndRootOnly — Regression guarded: an empty or run.started-only
// log must project a single root span without error (no nil-deref on rs==nil).
func TestProjectEmptyAndRootOnly(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	cases := []struct {
		name   string
		events []state.Event
	}{
		{"empty", nil},
		{"root_only", []state.Event{ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"})}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spans, err := Project(c.events, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if len(spans) != 1 || spans[0].Path != "" {
				t.Fatalf("%s: want exactly 1 root span (Path %q), got %d", c.name, "", len(spans))
			}
		})
	}
}
