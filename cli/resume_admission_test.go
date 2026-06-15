package cli

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func ev(t string, data string) state.Event { return state.Event{Type: t, Data: []byte(data)} }

func TestResumeAdmission(t *testing.T) {
	started := ev(engine.EventRunStarted, `{}`)
	cases := []struct {
		name      string
		events    []state.Event
		admit     bool
		label     string
		msgSubstr string // checked only when !admit
	}{
		{"interrupted", []state.Event{started}, true, "", ""},
		{"finished-ok", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"ok"}`)}, false, "", "Nothing to resume"},
		{"permanent", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"permanent_failure"}`)}, true, "permanent_failure", ""},
		{"rejected", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"rejected"}`)}, true, "rejected", ""},
		{"retryable-finished", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"retryable_failure"}`)}, true, "retryable_failure", ""},
		{"crashwindow-retryable", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"retryable_failure"}`)}, true, "", ""},
		{"crashwindow-permanent", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"permanent_failure"}`)}, true, "", ""},
		{"cancelled", []state.Event{started, ev(engine.EventRunCancelled, `{}`)}, true, "cancelled", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admit, msg, label := resumeAdmission("run-x", tc.events)
			if admit != tc.admit {
				t.Fatalf("admit = %v, want %v (msg=%q)", admit, tc.admit, msg)
			}
			if admit && label != tc.label {
				t.Fatalf("label = %q, want %q", label, tc.label)
			}
			if !admit && !strings.Contains(msg, tc.msgSubstr) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.msgSubstr)
			}
		})
	}
}
