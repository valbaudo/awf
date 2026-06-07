package graph

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func TestStateFromSpan(t *testing.T) {
	cases := []struct {
		name string
		span obs.Span
		want NodeState
	}{
		{
			name: "running (pending)",
			span: obs.Span{Pending: true, Attributes: map[string]any{obs.AttrNodeOutcome: "incomplete"}},
			want: NodeState{State: "running", Outcome: "incomplete"},
		},
		{
			name: "failed",
			span: obs.Span{Status: obs.StatusError, Attributes: map[string]any{obs.AttrNodeOutcome: "permanent_failure"}},
			want: NodeState{State: "failed", Outcome: "permanent_failure"},
		},
		{
			name: "skipped",
			span: obs.Span{Status: obs.StatusOK, Attributes: map[string]any{obs.AttrNodeOutcome: "skipped"}},
			want: NodeState{State: "skipped", Outcome: "skipped"},
		},
		{
			name: "completed",
			span: obs.Span{Status: obs.StatusOK, Attributes: map[string]any{obs.AttrNodeOutcome: "ok"}},
			want: NodeState{State: "completed", Outcome: "ok"},
		},
		{
			name: "completed no outcome attr",
			span: obs.Span{Status: obs.StatusOK, Attributes: map[string]any{}},
			want: NodeState{State: "completed"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateFromSpan(c.span); got != c.want {
				t.Errorf("stateFromSpan = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestOverlayIntegration builds a small event log (completed, failed, running, skipped)
// and asserts Overlay maps each node path to the right state. The "running" node has a
// node.started with no terminal event — exactly the live-run case the visual overlay
// depends on (RunState alone could not represent it).
func TestOverlayIntegration(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(off int, typ, path string, data any) state.Event {
		var raw json.RawMessage
		if data != nil {
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			raw = b
		}
		return state.Event{TS: base.Add(time.Duration(off) * time.Second), Type: typ, Path: path, Data: raw}
	}

	events := []state.Event{
		ev(0, engine.EventRunStarted, "", engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", WorkflowID: "wf"}),
		ev(1, engine.EventNodeStarted, "build", engine.NodeStartedData{Kind: "code"}),
		ev(2, engine.EventNodeCompleted, "build", engine.NodeCompletedData{Outcome: "ok"}),
		ev(3, engine.EventNodeStarted, "scan", engine.NodeStartedData{Kind: "agent"}),
		ev(4, engine.EventNodeFailed, "scan", engine.NodeFailedData{Outcome: "permanent_failure", Error: "boom"}),
		ev(5, engine.EventNodeStarted, "watch", engine.NodeStartedData{Kind: "agent"}), // no terminal → running
		ev(6, engine.EventNodeSkipped, "skipme", engine.NodeSkippedData{Reason: "n/a"}),
	}

	got, err := Overlay(events)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]NodeState{
		"build":  {State: "completed", Outcome: "ok"},
		"scan":   {State: "failed", Outcome: "permanent_failure"},
		"watch":  {State: "running", Outcome: "incomplete"},
		"skipme": {State: "skipped", Outcome: "skipped"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overlay mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if _, ok := got[""]; ok {
		t.Errorf("overlay must exclude the run-root span, got key \"\"")
	}
}
