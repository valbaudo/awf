package obs

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestProjectSynthesizesGateScopeSpans(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	// One gate, attempt-1, generate.exploit step.
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "gate[0].attempt-1.generate.exploit", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "gate[0].attempt-1.generate.exploit", t0.Add(5*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	spans, _ := Project(events, nil)

	// Every ancestor path must exist as a synthesized span.
	for _, want := range []string{"gate[0]", "gate[0].attempt-1", "gate[0].attempt-1.generate"} {
		s, ok := findSpan(spans, want)
		if !ok {
			t.Fatalf("no synthesized span for %q; got paths %v", want, paths(spans))
		}
		// Scope span bounds enclose the descendant leaf.
		if s.Start.After(t0.Add(1*time.Second)) || s.End.Before(t0.Add(5*time.Second)) {
			t.Errorf("scope %q bounds %v..%v don't enclose leaf +1s..+5s", want, s.Start, s.End)
		}
	}

	// Parent links: leaf → generate → attempt-1 → gate[0] → "" (root parent).
	leaf, _ := findSpan(spans, "gate[0].attempt-1.generate.exploit")
	if leaf.ParentPath != "gate[0].attempt-1.generate" {
		t.Errorf("leaf parent = %q", leaf.ParentPath)
	}
	gate, _ := findSpan(spans, "gate[0]")
	if gate.ParentPath != "" {
		t.Errorf("gate parent = %q, want root", gate.ParentPath)
	}
	if gate.Kind != "gate" || !gate.Scope {
		t.Errorf("gate span = kind %q scope %v, want gate/true", gate.Kind, gate.Scope)
	}
	// M1: scopes carry awf.scope.kind, NOT awf.node.kind.
	if gate.Attributes[AttrScopeKind] != "gate" {
		t.Errorf("gate awf.scope.kind = %v, want gate", gate.Attributes[AttrScopeKind])
	}
	if _, has := gate.Attributes[AttrNodeKind]; has {
		t.Errorf("scope span must NOT carry awf.node.kind; got %v", gate.Attributes[AttrNodeKind])
	}
	// The leaf step, by contrast, carries awf.node.kind and is not a scope.
	if leaf.Scope || leaf.Attributes[AttrNodeKind] != "agent" {
		t.Errorf("leaf span = scope %v node.kind %v, want false/agent", leaf.Scope, leaf.Attributes[AttrNodeKind])
	}
}

func paths(spans []Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Path
	}
	return out
}
