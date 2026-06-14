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
		force     bool
		admit     bool
		label     string
		msgSubstr string // checked only when !admit
	}{
		{"interrupted-noforce", []state.Event{started}, false, true, "", ""},
		{"interrupted-force", []state.Event{started}, true, true, "", ""},
		{"finished-ok-noforce", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"ok"}`)}, false, false, "", "already finished"},
		{"finished-ok-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"ok"}`)}, true, false, "", "already finished"},
		{"permanent-noforce", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"permanent_failure"}`)}, false, false, "", "--force"},
		{"permanent-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"permanent_failure"}`)}, true, true, "permanent_failure", ""},
		{"rejected-noforce", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"rejected"}`)}, false, false, "", "--force"},
		{"rejected-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"rejected"}`)}, true, true, "rejected", ""},
		{"retryable-finished-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"retryable_failure"}`)}, true, true, "retryable_failure", ""},
		{"crashwindow-retryable-force", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"retryable_failure"}`)}, true, true, "retryable_failure", ""},
		{"cancelled-noforce", []state.Event{started, ev(engine.EventRunCancelled, `{}`)}, false, false, "", "cancelled"},
		{"cancelled-force", []state.Event{started, ev(engine.EventRunCancelled, `{}`)}, true, true, "cancelled", ""},
		{"crashwindow-permanent-force", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"permanent_failure"}`)}, true, true, "permanent_failure", ""},
		{"crashwindow-permanent-noforce", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"permanent_failure"}`)}, false, false, "", "--force"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admit, msg, label := resumeAdmission("run-x", tc.events, tc.force)
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
