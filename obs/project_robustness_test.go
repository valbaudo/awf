package obs

import (
	"strings"
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
		{"skills.selected", state.Event{Type: engine.EventSkillsSelected, Path: "hunt", TS: t0, Data: []byte("{")}},
		{"node.started", state.Event{Type: engine.EventNodeStarted, Path: "s1", TS: t0, Data: []byte("{not json")}},
		{"node.completed", state.Event{Type: engine.EventNodeCompleted, Path: "s1", TS: t0, Data: []byte("nope")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Project([]state.Event{rs, c.bad}, nil)
			if err == nil {
				t.Fatalf("Project: want error on malformed %s payload, got nil", c.name)
			}
			if c.name == engine.EventSkillsSelected {
				msg := err.Error()
				if !strings.Contains(msg, engine.EventSkillsSelected) || !strings.Contains(msg, c.bad.Path) {
					t.Fatalf("Project error = %q, want mention %q and path %q", msg, engine.EventSkillsSelected, c.bad.Path)
				}
			}
		})
	}
}

func TestProjectSkillsSelectedRejectsEmptyPath(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
		ev(t, engine.EventSkillsSelected, "", t0.Add(time.Second), engine.SkillsSelectedData{
			Library:       "stdlib",
			LibraryDigest: "sha256:abc123",
			Router:        "tfidf",
			RouterVersion: "v1",
			Selected:      []engine.SelectedSkill{{ID: "skills/search", Score: 0.9}},
		}),
	}
	_, err := Project(events, nil)
	if err == nil {
		t.Fatal("Project: want error on skills.selected with empty path, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, engine.EventSkillsSelected) || !strings.Contains(msg, "empty path") {
		t.Fatalf("Project error = %q, want mention %q and empty path", msg, engine.EventSkillsSelected)
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
