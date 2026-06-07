package graph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// TestKindOfExhaustive pins the kind string for every one of ir.Node's 10 kinds. If a
// new kind is added to ir without a case here (and in kindOf), this is the first place
// it should be noticed; kindOf itself panics on an unmapped kind.
func TestKindOfExhaustive(t *testing.T) {
	cases := []struct {
		n    ir.Node
		want string
	}{
		{&ir.CodeStep{}, "code"},
		{&ir.AgentStep{}, "agent"},
		{&ir.SignalStep{}, "signal"},
		{&ir.If{}, "if"},
		{&ir.Loop{}, "loop"},
		{&ir.Try{}, "try"},
		{&ir.Parallel{}, "parallel"},
		{&ir.Gate{}, "gate"},
		{&ir.Skip{}, "skip"},
		{&ir.Map{}, "map"},
	}
	if len(cases) != 10 {
		t.Fatalf("expected 10 node kinds, listed %d", len(cases))
	}
	for _, c := range cases {
		if got := kindOf(c.n); got != c.want {
			t.Errorf("kindOf(%T) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestBuildStaticStructure checks paths, kinds, ids, parents, control edges, and opaque
// `with:` passthrough across nested control nodes (gate + map).
func TestBuildStaticStructure(t *testing.T) {
	wf := &ir.Workflow{
		ID: "demo",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "build", Run: "make"},
			&ir.Gate{
				Generate:    ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "x", With: ir.RawConfig{"model": "x"}}},
				Evaluate:    ir.NodeList{&ir.CodeStep{ID: "check", Run: "test"}},
				Until:       "ok",
				MaxAttempts: 2,
			},
			&ir.Map{
				Over:      "items",
				As:        "it",
				Container: "c",
				Body:      ir.NodeList{&ir.CodeStep{ID: "work", Run: "go"}},
			},
		},
	}

	got := BuildStatic(wf)

	want := Projection{
		SchemaVersion: SchemaVersion,
		Workflow:      "demo",
		Nodes: []Node{
			{Path: "build", Kind: "code", ID: "build", Parent: "", NodeClass: "template"},
			{Path: "gate[1]", Kind: "gate", Parent: "", NodeClass: "template"},
			{Path: "gate[1].generate.gen", Kind: "agent", ID: "gen", Parent: "gate[1].generate", With: ir.RawConfig{"model": "x"}, NodeClass: "template"},
			{Path: "gate[1].evaluate.check", Kind: "code", ID: "check", Parent: "gate[1].evaluate", NodeClass: "template"},
			{Path: "map[2]", Kind: "map", Parent: "", NodeClass: "template"},
			{Path: "map[2].body.work", Kind: "code", ID: "work", Parent: "map[2].body", NodeClass: "template"},
		},
		Edges: []Edge{
			{From: "build", To: "gate[1]", Kind: "control"},
			{From: "gate[1]", To: "map[2]", Kind: "control"},
		},
	}

	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		wj, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("projection mismatch:\n got=%s\nwant=%s", gj, wj)
	}
}

// TestBuildStaticDataEdges verifies producer->consumer data edges are derived from
// templated fields (a {{ }} run ref and a bare map.over expr), reusing the template parser.
func TestBuildStaticDataEdges(t *testing.T) {
	wf := &ir.Workflow{
		ID: "d",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "build", Run: "make"},
			&ir.CodeStep{ID: "analyze", Run: "cat {{ step.build.output.path }}"},
			&ir.Map{Over: "step.build.output.items", As: "it", Container: "c", Body: ir.NodeList{&ir.CodeStep{ID: "w", Run: "go"}}},
		},
	}
	got := BuildStatic(wf)
	has := func(e Edge) bool {
		for _, x := range got.Edges {
			if x == e {
				return true
			}
		}
		return false
	}
	// {{ step.build... }} in analyze.run -> data edge build->analyze.
	if !has(Edge{From: "build", To: "analyze", Kind: "data"}) {
		t.Errorf("missing data edge build->analyze; edges=%+v", got.Edges)
	}
	// map.over references build -> data edge build->map[2].
	if !has(Edge{From: "build", To: "map[2]", Kind: "data"}) {
		t.Errorf("missing data edge build->map[2]; edges=%+v", got.Edges)
	}
	// control edges still present alongside data edges.
	if !has(Edge{From: "build", To: "analyze", Kind: "control"}) {
		t.Errorf("missing control edge build->analyze; edges=%+v", got.Edges)
	}
}

// TestBuildStaticSkipOmitted verifies a `skip` node is omitted and the control edge
// connects the surrounding siblings directly.
func TestBuildStaticSkipOmitted(t *testing.T) {
	wf := &ir.Workflow{
		ID: "s",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "a", Run: "x"},
			&ir.Skip{Reason: "nope"},
			&ir.CodeStep{ID: "b", Run: "y"},
		},
	}
	got := BuildStatic(wf)

	if len(got.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (skip omitted): %+v", len(got.Nodes), got.Nodes)
	}
	for _, n := range got.Nodes {
		if n.Kind == "skip" {
			t.Errorf("skip node should be omitted, found %+v", n)
		}
	}
	if len(got.Edges) != 1 || got.Edges[0] != (Edge{From: "a", To: "b", Kind: "control"}) {
		t.Errorf("want single control edge a->b, got %+v", got.Edges)
	}
}

// TestBuildStaticDeterministic asserts the JSON is byte-identical across two builds of
// the same workflow (determinism is a project invariant; golden tests depend on it).
func TestBuildStaticDeterministic(t *testing.T) {
	wf := &ir.Workflow{
		ID: "d",
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "z", Uses: "u", With: ir.RawConfig{"b": 2, "a": 1, "c": 3}},
			&ir.CodeStep{ID: "y", Run: "r"},
		},
	}
	a, err := json.Marshal(BuildStatic(wf))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(BuildStatic(wf))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("non-deterministic output:\n a=%s\n b=%s", a, b)
	}
	// schema_version present, with: keys serialized (opaque passthrough).
	if !strings.Contains(string(a), `"schema_version":2`) {
		t.Errorf("missing schema_version: %s", a)
	}
	if !strings.Contains(string(a), `"with":{"a":1,"b":2,"c":3}`) {
		t.Errorf("with: not passed through opaque/sorted: %s", a)
	}
}

// TestBuildStaticEmpty checks nil / empty workflows yield a well-formed projection with
// non-nil (JSON `[]`) node/edge slices.
func TestBuildStaticEmpty(t *testing.T) {
	got := BuildStatic(nil)
	if got.SchemaVersion != SchemaVersion || got.Workflow != "" {
		t.Errorf("unexpected header: %+v", got)
	}
	if got.Nodes == nil || got.Edges == nil {
		t.Errorf("nodes/edges must be non-nil, got nodes=%v edges=%v", got.Nodes, got.Edges)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"nodes":[]`) || !strings.Contains(string(b), `"edges":[]`) {
		t.Errorf("want empty arrays in JSON, got %s", b)
	}
	if strings.Contains(string(b), `run_overlay`) {
		t.Errorf("run_overlay should be omitted when absent: %s", b)
	}
}
