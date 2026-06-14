package obs

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestDeriveStatus(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	mk := func(events ...state.Event) []state.Event { return events }

	cases := []struct {
		name   string
		events []state.Event
		want   RunStatus
	}{
		{"finished-ok", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "ok"}),
		), RunFinished},
		{"failed-outcome", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "permanent_failure"}),
		), RunFailed},
		{"terminal-node-failed", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventNodeStarted, "s1", t0.Add(time.Second), engine.NodeStartedData{Kind: "code"}),
			ev(t, engine.EventNodeFailed, "s1", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "permanent_failure", Error: "boom"}),
		), RunFailed},
		{"cancelled", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunCancelled, "", t0.Add(time.Second), engine.RunCancelledData{}),
		), RunCancelled},
		{"paused", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunPaused, "", t0.Add(time.Second), engine.RunPausedData{}),
		), RunPaused},
		{"paused-then-resumed", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunPaused, "", t0.Add(time.Second), engine.RunPausedData{}),
			ev(t, engine.EventRunResumed, "", t0.Add(2*time.Second), engine.RunResumedData{Epoch: 2}),
		), RunIncomplete},
		{"incomplete-started-no-terminal", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventNodeStarted, "s1", t0.Add(time.Second), engine.NodeStartedData{Kind: "agent"}),
		), RunIncomplete},
		{"empty-log", mk(), RunIncomplete},
		{"resumable-run-finished", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "retryable_failure"}),
		), RunResumable},
		{"failed-permanent-run-finished", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "rejected"}),
		), RunFailed},
		{"resumable-terminal-node-failed", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventNodeStarted, "s1", t0.Add(time.Second), engine.NodeStartedData{Kind: "code"}),
			ev(t, engine.EventNodeFailed, "s1", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "retryable_failure", Error: "transient"}),
		), RunResumable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveStatus(c.events); got != c.want {
				t.Errorf("DeriveStatus = %q, want %q", got, c.want)
			}
		})
	}
}
