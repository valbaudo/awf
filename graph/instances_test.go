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

	// The map container stays; template body steps that have instances are dropped in the
	// run view (the instances represent them).
	if byPath["map[0]"].NodeClass != "template" {
		t.Errorf("map[0] container missing/mislabeled: %+v", byPath["map[0]"])
	}
	if _, ok := byPath["map[0].body.work"]; ok {
		t.Errorf("template body step map[0].body.work should be dropped in run view (covered by instances)")
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

func TestBuildWithRunLoadedMapsCallChildOverlay(t *testing.T) {
	child := &ir.Workflow{
		ID: "child",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "leaf", Run: "true"},
		},
	}
	root := &ir.Workflow{
		ID: "root",
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "child"},
		},
	}
	ld := &ir.LoadedDefinition{
		Workflow: root,
		Modules: map[string]*ir.LoadedModule{
			"":          {ID: "", Workflow: root},
			"mod-child": {ID: "mod-child", Workflow: child},
		},
		ImportEdges: []ir.LoadedImportEdge{{ParentID: "", ImportID: "child", ChildID: "mod-child"}},
	}
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	events := []state.Event{
		{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1"})},
		{Type: engine.EventCallStarted, Path: "recon", Data: d(engine.CallStartedData{InputRef: "sha256:call-input"})},
		{Type: engine.EventNodeStarted, Path: "recon.workflow.leaf", Data: d(engine.NodeStartedData{Kind: "code"})},
		{Type: engine.EventNodeFailed, Path: "recon.workflow.leaf", Data: d(engine.NodeFailedData{Outcome: "permanent_failure", Error: "boom"})},
		{Type: engine.EventNodeFailed, Path: "recon", Data: d(engine.NodeFailedData{Outcome: "permanent_failure", Error: "call failed"})},
	}

	p, err := BuildWithRunLoaded(ld, events)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Node{}
	for _, n := range p.Nodes {
		byPath[n.Path] = n
	}
	if n := byPath["recon.workflow.leaf"]; n.Kind != "code" || n.NodeClass != "template" {
		t.Fatalf("missing static child template node under call: %+v", n)
	}
	if st := p.RunOverlay["recon.workflow.leaf"]; st.State != "failed" || st.Outcome != "permanent_failure" {
		t.Errorf("child overlay = %+v, want failed/permanent_failure", st)
	}
	if st := p.RunOverlay["recon"]; st.State != "failed" || st.Outcome != "permanent_failure" {
		t.Errorf("call overlay = %+v, want failed/permanent_failure", st)
	}
}
