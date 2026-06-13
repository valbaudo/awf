package graph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// TestKindOfExhaustive pins the kind string for every one of ir.Node's 13 kinds. If a
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
		{&ir.CallStep{}, "call"},
		{&ir.If{}, "if"},
		{&ir.Loop{}, "loop"},
		{&ir.Try{}, "try"},
		{&ir.Parallel{}, "parallel"},
		{&ir.Gate{}, "gate"},
		{&ir.Skip{}, "skip"},
		{&ir.Map{}, "map"},
		{&ir.Compose{}, "compose"},
		{&ir.React{ID: "r"}, "react"}, // P3 A3 (keep in sync with ir/node_test.go wantKinds)
	}
	if len(cases) != 13 {
		t.Fatalf("expected 13 node kinds, listed %d", len(cases))
	}
	for _, c := range cases {
		if got := kindOf(c.n); got != c.want {
			t.Errorf("kindOf(%T) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestReactGraphArms is the regression that conformance can't reach: it forces the
// graph package's react arms (kindOf, staticPath) directly. A top-level react node's
// static path is react[i] (keyword[index], not its id) — the Map-class addressing.
func TestReactGraphArms(t *testing.T) {
	if got := kindOf(&ir.React{ID: "ans"}); got != "react" {
		t.Errorf("kindOf(*ir.React) = %q, want %q", got, "react")
	}
	if got := staticPath("", &ir.React{ID: "ans"}, 0); got != "react[0]" {
		t.Errorf("staticPath(top-level react @0) = %q, want %q", got, "react[0]")
	}
	if got := staticPath("loop[1].body", &ir.React{ID: "ans"}, 2); got != "loop[1].body.react[2]" {
		t.Errorf("staticPath(nested react) = %q, want %q", got, "loop[1].body.react[2]")
	}
}

func hasEdge(p Projection, want Edge) bool {
	for _, e := range p.Edges {
		if e == want {
			return true
		}
	}
	return false
}

func dataEdgesTo(p Projection, to string) []Edge {
	var out []Edge
	for _, e := range p.Edges {
		if e.Kind == "data" && e.To == to {
			out = append(out, e)
		}
	}
	return out
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

func TestBuildStaticCallInputDataEdge(t *testing.T) {
	wf := &ir.Workflow{
		ID: "call-data",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "prep", Run: "true"},
			&ir.CallStep{
				ID:   "recon",
				Call: "child",
				Input: map[string]ir.TemplateValue{
					"target": []byte(`"{{ step.prep.finding }}"`),
				},
			},
		},
	}

	got := BuildStatic(wf)

	if !hasEdge(got, Edge{From: "prep", To: "recon", Kind: "data"}) {
		t.Errorf("missing call input data edge prep->recon; edges=%+v", got.Edges)
	}
}

func TestGraphDataEdgeFromCallInputFiles(t *testing.T) {
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"child": "child.awf.yaml",
		},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://root@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:          "collect",
				Container:   "c",
				Run:         "true",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.json"}},
			},
			&ir.CallStep{
				ID:         "analyze",
				Call:       "child",
				InputFiles: map[string]string{"report": "step.collect.files.report"},
			},
		},
	}

	got := BuildStatic(root)
	want := Edge{From: "collect", To: "analyze", Kind: "data"}
	if !hasEdge(got, want) {
		t.Fatalf("missing data edge %+v; edges=%+v", want, got.Edges)
	}
}

func TestBuildStaticCallInputFilesDataEdgesDeterministic(t *testing.T) {
	build := func() []byte {
		wf := &ir.Workflow{
			ID: "call-input-files-determinism",
			Graph: ir.NodeList{
				&ir.CodeStep{ID: "a", Run: "true", OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/a.json"}}},
				&ir.CodeStep{ID: "b", Run: "true", OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/b.json"}}},
				&ir.CodeStep{ID: "c", Run: "true", OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/c.json"}}},
				&ir.CodeStep{ID: "d", Run: "true", OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/d.json"}}},
				&ir.CallStep{
					ID:   "recon",
					Call: "child",
					InputFiles: map[string]string{
						"alpha":   "step.a.files.report",
						"bravo":   "step.b.files.report",
						"charlie": "step.c.files.report",
						"delta":   "step.d.files.report",
					},
				},
			},
		}
		b, err := json.Marshal(BuildStatic(wf))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	want := string(build())
	for i := 0; i < 200; i++ {
		if got := string(build()); got != want {
			t.Fatalf("BuildStatic call-input-files graph JSON changed across repeated builds:\nfirst=%s\nlater=%s", want, got)
		}
	}
}

func TestBuildStaticNestedCallInputDataEdgeUsesImportedModuleIndex(t *testing.T) {
	root := &ir.Workflow{
		ID: "root",
		Graph: ir.NodeList{
			&ir.CallStep{ID: "outer", Call: "child"},
		},
	}
	child := &ir.Workflow{
		ID: "child",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "prep", Run: "true"},
			&ir.CallStep{
				ID:   "inner",
				Call: "grand",
				Input: map[string]ir.TemplateValue{
					"target": []byte(`{"finding":"{{ step.prep.finding }}"}`),
				},
			},
		},
	}
	grand := &ir.Workflow{ID: "grand"}
	ld := &ir.LoadedDefinition{
		Workflow: root,
		Modules: map[string]*ir.LoadedModule{
			"":          {ID: "", Workflow: root},
			"mod-child": {ID: "mod-child", Workflow: child},
			"mod-grand": {ID: "mod-grand", Workflow: grand},
		},
		ImportEdges: []ir.LoadedImportEdge{
			{ParentID: "", ImportID: "child", ChildID: "mod-child"},
			{ParentID: "mod-child", ImportID: "grand", ChildID: "mod-grand"},
		},
	}

	got := BuildStaticLoaded(ld)

	if !hasEdge(got, Edge{From: "outer.workflow.prep", To: "outer.workflow.inner", Kind: "data"}) {
		t.Errorf("missing nested call input data edge outer.workflow.prep->outer.workflow.inner; edges=%+v", got.Edges)
	}
}

func TestBuildStaticCallInputDataEdgesDeterministic(t *testing.T) {
	build := func() []byte {
		wf := &ir.Workflow{
			ID: "call-input-determinism",
			Graph: ir.NodeList{
				&ir.CodeStep{ID: "a", Run: "true"},
				&ir.CodeStep{ID: "b", Run: "true"},
				&ir.CodeStep{ID: "c", Run: "true"},
				&ir.CodeStep{ID: "d", Run: "true"},
				&ir.CallStep{
					ID:   "recon",
					Call: "child",
					Input: map[string]ir.TemplateValue{
						"alpha": []byte(`"{{ step.a.output }}"`),
						"bravo": []byte(`"{{ step.b.output }}"`),
						"delta": []byte(`"{{ step.d.output }}"`),
						"charlie": []byte(`{
							"finding": "{{ step.c.output }}",
							"dupe": "{{ step.a.output }}"
						}`),
					},
				},
			},
		}
		b, err := json.Marshal(BuildStatic(wf))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	want := string(build())
	for i := 0; i < 200; i++ {
		if got := string(build()); got != want {
			t.Fatalf("BuildStatic call-input graph JSON changed across repeated builds:\nfirst=%s\nlater=%s", want, got)
		}
	}
}

func TestBuildStaticCallInputNestedObjectDataEdgesSorted(t *testing.T) {
	wf := &ir.Workflow{
		ID: "call-input-nested-order",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "c", Run: "true"},
			&ir.CodeStep{ID: "d", Run: "true"},
			&ir.CallStep{
				ID:   "recon",
				Call: "child",
				Input: map[string]ir.TemplateValue{
					"payload": []byte(`{
						"x": "{{ step.c.output }}",
						"y": "{{ step.d.output }}"
					}`),
				},
			},
		},
	}

	want := []Edge{
		{From: "c", To: "recon", Kind: "data"},
		{From: "d", To: "recon", Kind: "data"},
	}
	for i := 0; i < 200; i++ {
		if got := dataEdgesTo(BuildStatic(wf), "recon"); !reflect.DeepEqual(got, want) {
			t.Fatalf("call input nested object data edges = %+v, want %+v", got, want)
		}
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

func TestGraphIncludesImportedChildNodesUnderCall(t *testing.T) {
	ld := &ir.LoadedDefinition{
		Workflow: &ir.Workflow{
			ID: "root",
			Graph: ir.NodeList{
				&ir.CodeStep{ID: "prep", Run: "true"},
				&ir.CallStep{ID: "recon", Call: "child"},
			},
		},
		Modules: map[string]*ir.LoadedModule{
			"": {
				ID: "",
				Workflow: &ir.Workflow{
					ID: "root",
					Graph: ir.NodeList{
						&ir.CodeStep{ID: "prep", Run: "true"},
						&ir.CallStep{ID: "recon", Call: "child"},
					},
				},
			},
			"mod-child": {
				ID: "mod-child",
				Workflow: &ir.Workflow{
					ID: "child",
					Graph: ir.NodeList{
						&ir.CodeStep{ID: "leaf", Run: "true"},
					},
				},
			},
		},
		ImportEdges: []ir.LoadedImportEdge{{ParentID: "", ImportID: "child", ChildID: "mod-child"}},
	}

	got := BuildStaticLoaded(ld)
	byPath := map[string]Node{}
	for _, n := range got.Nodes {
		byPath[n.Path] = n
	}
	if n := byPath["recon"]; n.Kind != "call" || n.ID != "recon" || n.Parent != "" || n.NodeClass != "template" {
		t.Errorf("call node recon = %+v, want call template", n)
	}
	if n := byPath["recon.workflow.leaf"]; n.Kind != "code" || n.ID != "leaf" || n.Parent != "recon.workflow" || n.NodeClass != "template" {
		t.Errorf("imported child node = %+v, want code leaf under recon.workflow", n)
	}
	hasControl := false
	for _, e := range got.Edges {
		if e == (Edge{From: "prep", To: "recon", Kind: "control"}) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		t.Errorf("missing root control edge prep->recon; edges=%+v", got.Edges)
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
