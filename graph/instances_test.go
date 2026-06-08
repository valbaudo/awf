package graph

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestTemplateOfAndContext(t *testing.T) {
	cases := []struct{ runtime, tmpl, ctx string }{
		{"build", "build", ""},
		{"map[0].item-3.work", "map[0].body.work", "item-3"},                    // map: item replaces body
		{"gate[2].attempt-1.generate.gen", "gate[2].generate.gen", "attempt-1"}, // gate: attempt appended
		{"loop[0].body.iter-2.step", "loop[0].body.step", "iter-2"},             // loop: iter appended
		// nested expansion (e.g. a loop inside a map item): the graph layer drops every
		// instance segment and collects them all, so it renders these if they ever appear.
		{"map[0].item-1.loop[0].body.iter-2.step", "map[0].body.loop[0].body.step", "item-1.iter-2"},
	}
	for _, c := range cases {
		if got := templateOf(c.runtime); got != c.tmpl {
			t.Errorf("templateOf(%q)=%q, want %q", c.runtime, got, c.tmpl)
		}
		if got := instanceContext(c.runtime); got != c.ctx {
			t.Errorf("instanceContext(%q)=%q, want %q", c.runtime, got, c.ctx)
		}
	}
}

// TestBuildWithRunInstances: a 2-item map run produces instance nodes for each item scope
// and its body-step instances, with node_class/instance_of set, runtime control edges
// projected per item, and run state in the overlay.
func TestBuildWithRunInstances(t *testing.T) {
	wf := &ir.Workflow{
		ID: "m",
		Graph: ir.NodeList{
			&ir.Map{Over: "input.items", As: "it", Container: "c", Body: ir.NodeList{
				&ir.CodeStep{ID: "work", Run: "a"},
				&ir.CodeStep{ID: "score", Run: "b"},
			}},
		},
	}
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	ev := func(typ, path string, data any) state.Event {
		return state.Event{Type: typ, Path: path, Data: d(data)}
	}
	var events []state.Event
	events = append(events, ev(engine.EventRunStarted, "", engine.RunStartedData{RunID: "r1"}))
	for _, item := range []string{"item-0", "item-1"} {
		for _, step := range []string{"work", "score"} {
			p := "map[0]." + item + "." + step
			events = append(events,
				ev(engine.EventNodeStarted, p, engine.NodeStartedData{Kind: "code"}),
				ev(engine.EventNodeCompleted, p, engine.NodeCompletedData{Outcome: "ok"}),
			)
		}
	}

	p, err := BuildWithRun(wf, events)
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]Node{}
	for _, n := range p.Nodes {
		byPath[n.Path] = n
	}

	// Template nodes still present.
	if byPath["map[0]"].NodeClass != "template" || byPath["map[0].body.work"].NodeClass != "template" {
		t.Errorf("template nodes missing/mislabeled: %+v", byPath)
	}
	// Instance scope for item-0 — instance_of resolves to the enclosing map node.
	if n := byPath["map[0].item-0"]; n.NodeClass != "instance" || n.Kind != "map_item" || n.Parent != "map[0]" || n.InstanceOf != "map[0]" {
		t.Errorf("item-0 scope = %+v, want instance/map_item/parent map[0]/instance_of map[0]", n)
	}
	// Instance leaf, pointing back at its template step.
	if n := byPath["map[0].item-1.score"]; n.NodeClass != "instance" || n.InstanceOf != "map[0].body.score" || n.Parent != "map[0].item-1" {
		t.Errorf("item-1.score = %+v, want instance/instance_of map[0].body.score/parent map[0].item-1", n)
	}
	// Runtime control edge work->score projected into each item.
	hasEdge := func(from, to string) bool {
		for _, e := range p.Edges {
			if e.From == from && e.To == to && e.Kind == "control" {
				return true
			}
		}
		return false
	}
	if !hasEdge("map[0].item-0.work", "map[0].item-0.score") {
		t.Errorf("missing runtime edge item-0 work->score; edges=%+v", p.Edges)
	}
	if !hasEdge("map[0].item-1.work", "map[0].item-1.score") {
		t.Errorf("missing runtime edge item-1 work->score; edges=%+v", p.Edges)
	}
	// Overlay carries state for an instance leaf.
	if st := p.RunOverlay["map[0].item-0.work"].State; st != "completed" {
		t.Errorf("overlay item-0.work = %q, want completed", st)
	}
}
