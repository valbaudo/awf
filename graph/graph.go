// Package graph projects a workflow into a node/edge graph for visualization — the
// JSON contract behind a future visual graph tool (`awf graph --json`). It is a pure,
// read-only projection: static structure comes from walking ir.Graph; optional
// per-node run state comes from Overlay (obs.Project), keyed by runtime path.
//
// Slice 1 scope (see docs/.gstack design + TODOS.md): static template graph + a FLAT
// run_overlay keyed by runtime path. There are no first-class runtime "instance" nodes
// (map item / gate attempt / loop iter) — that object model is deferred to the UI slice.
//
// `Skip` nodes are omitted from the static graph: a `skip` carries no addressable
// identity and is absent from the §8 addressing grammar (same as ir.WalkNodes). A
// `skip:` between two siblings therefore does not render as a node; the control edge
// connects the surrounding nodes directly.
package graph

import (
	"fmt"

	"github.com/valbaudo/awf/ir"
)

// SchemaVersion is the version of the projection JSON contract. This JSON is a stable
// boundary (consumed by external tooling), so every emission carries it — bump on any
// breaking shape change. v2 added data edges (Edge.Kind "data"), node_class, instance_of,
// and runtime instance nodes (see instances.go).
const SchemaVersion = 2

// Projection is the full graph JSON. Nodes and Edges are emitted in deterministic
// walk order (NodeList declaration order). RunOverlay, when present, is keyed by
// runtime path and JSON-marshals with sorted keys (encoding/json sorts map keys), so
// the whole document is byte-deterministic for a given input.
type Projection struct {
	SchemaVersion int                  `json:"schema_version"`
	Workflow      string               `json:"workflow"`
	Nodes         []Node               `json:"nodes"`
	Edges         []Edge               `json:"edges"`
	RunOverlay    map[string]NodeState `json:"run_overlay,omitempty"`
}

// Node is one template node in the static graph. Path is the static IR path (the same
// string the runtime uses, minus per-iteration suffixes). Parent is the immediate
// enclosing addressing scope: "" at the top level, a node path for parallel children
// (bare `parallel[i]`, no branch label), or a branch-scope path for branch children
// (e.g. `gate[1].generate`, `loop[0].body`, `try[0].catch`). With carries an agent
// step's opaque `with:` config verbatim — never interpreted (the RawConfig invariant).
type Node struct {
	Path   string       `json:"path"`
	Kind   string       `json:"kind"`
	ID     string       `json:"id,omitempty"`
	Parent string       `json:"parent,omitempty"`
	With   ir.RawConfig `json:"with,omitempty"`
	// NodeClass is "template" (a node authored in the graph) or "instance" (a runtime
	// expansion that exists only for a given run — a map item, gate attempt, or loop
	// iteration and its children). InstanceOf, set only on instances, is the path of the
	// template node this instance is a copy of.
	NodeClass  string `json:"node_class,omitempty"`
	InstanceOf string `json:"instance_of,omitempty"`
}

// Edge is a control-flow edge between consecutive sibling nodes within one addressing
// scope (slice/declaration order). Containment (parent → child) is carried by Node.Parent,
// not by edges. Data edges (from `{{ }}` refs) are a deferred fast-follow (no edge here yet).
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// NodeState is the per-path run state in RunOverlay. State is one of running / completed
// / failed / skipped. Outcome carries the raw awf.node.outcome attribute when present.
type NodeState struct {
	State   string `json:"state"`
	Outcome string `json:"outcome,omitempty"`
}

// BuildStatic projects a workflow's static structure (nodes + control edges). It never
// errors and never touches a run log. A nil workflow yields an empty (but well-formed)
// projection. Nodes/Edges are always non-nil so the JSON emits [] rather than null.
func BuildStatic(wf *ir.Workflow) Projection {
	return BuildStaticLoaded(&ir.LoadedDefinition{Workflow: wf})
}

// BuildStaticLoaded projects a loaded workflow definition, expanding imported child
// workflow templates under each concrete call site using runtime-shaped
// <call>.workflow.* paths.
func BuildStaticLoaded(ld *ir.LoadedDefinition) Projection {
	p := Projection{
		SchemaVersion: SchemaVersion,
		Nodes:         []Node{},
		Edges:         []Edge{},
	}
	if ld == nil {
		return p
	}
	wf := ld.Workflow
	if root := ld.Root(); root != nil && root.Workflow != nil {
		wf = root.Workflow
	}
	if wf != nil {
		p.Workflow = wf.ID
		walk(wf.Graph, "", &p, stepPathIndex(wf.Graph, ""), ld, "")
	}
	return p
}

// stepPathIndex maps every step id to its static node path, used to resolve data-edge
// references (`step.<id>.…`) to producer node paths. Reuses the canonical ir.WalkNodes.
func stepPathIndex(list ir.NodeList, parent string) map[string]string {
	idx := map[string]string{}
	walkNodes(list, parent, func(n ir.Node, path string) {
		switch n.(type) {
		case *ir.CodeStep, *ir.AgentStep, *ir.SignalStep, *ir.CallStep:
			idx[stepID(n)] = path
		}
	})
	return idx
}

func walkNodes(list ir.NodeList, parent string, visit func(ir.Node, string)) {
	for i, n := range list {
		if _, isSkip := n.(*ir.Skip); isSkip {
			continue
		}
		path := staticPath(parent, n, i)
		visit(n, path)
		switch v := n.(type) {
		case *ir.If:
			walkNodes(v.Then, ir.ChildPath(parent, "if", i, "then"), visit)
			walkNodes(v.Else, ir.ChildPath(parent, "if", i, "else"), visit)
		case *ir.Loop:
			walkNodes(v.Body, ir.ChildPath(parent, "loop", i, "body"), visit)
		case *ir.Try:
			walkNodes(v.Do, ir.ChildPath(parent, "try", i, "do"), visit)
			walkNodes(v.Catch, ir.ChildPath(parent, "try", i, "catch"), visit)
			walkNodes(v.Finally, ir.ChildPath(parent, "try", i, "finally"), visit)
		case *ir.Parallel:
			walkNodes(v.Children, path, visit)
		case *ir.Gate:
			walkNodes(v.Generate, ir.ChildPath(parent, "gate", i, "generate"), visit)
			walkNodes(v.Evaluate, ir.ChildPath(parent, "gate", i, "evaluate"), visit)
		case *ir.Map:
			walkNodes(v.Body, ir.ChildPath(parent, "map", i, "body"), visit)
		case *ir.Compose:
			walkNodes(v.Body, ir.ChildPath(parent, "compose", i, "body"), visit)
		}
	}
}

// walk emits one Node per non-Skip node in declaration order, a control Edge between
// consecutive emitted siblings, and recurses into each control node's child branches.
// Paths come from ir.PathFor / ir.ChildPath — the single source for the addressing
// grammar — so they can never drift from the runtime/validator forms.
func walk(list ir.NodeList, parent string, p *Projection, idx map[string]string, ld *ir.LoadedDefinition, moduleID string) {
	prev := ""
	havePrev := false
	for i, n := range list {
		if _, isSkip := n.(*ir.Skip); isSkip {
			continue // no addressable identity; omitted (see package doc)
		}
		path := staticPath(parent, n, i)
		p.Nodes = append(p.Nodes, Node{
			Path:      path,
			Kind:      kindOf(n),
			ID:        stepID(n),
			Parent:    parent,
			With:      withOf(n),
			NodeClass: "template",
		})
		if havePrev {
			p.Edges = append(p.Edges, Edge{From: prev, To: path, Kind: "control"})
		}
		prev, havePrev = path, true

		// Data edges: producer -> this node, for each `step.<id>` reference in this
		// node's templated fields whose producer resolves to a node path.
		for _, refID := range producerRefs(n) {
			if from, ok := idx[refID]; ok && from != path {
				p.Edges = append(p.Edges, Edge{From: from, To: path, Kind: "data"})
			}
		}

		switch v := n.(type) {
		case *ir.If:
			walk(v.Then, ir.ChildPath(parent, "if", i, "then"), p, idx, ld, moduleID)
			walk(v.Else, ir.ChildPath(parent, "if", i, "else"), p, idx, ld, moduleID)
		case *ir.Loop:
			walk(v.Body, ir.ChildPath(parent, "loop", i, "body"), p, idx, ld, moduleID)
		case *ir.Try:
			walk(v.Do, ir.ChildPath(parent, "try", i, "do"), p, idx, ld, moduleID)
			walk(v.Catch, ir.ChildPath(parent, "try", i, "catch"), p, idx, ld, moduleID)
			walk(v.Finally, ir.ChildPath(parent, "try", i, "finally"), p, idx, ld, moduleID)
		case *ir.Parallel:
			walk(v.Children, path, p, idx, ld, moduleID) // bare parallel[i] — no branch label
		case *ir.Gate:
			walk(v.Generate, ir.ChildPath(parent, "gate", i, "generate"), p, idx, ld, moduleID)
			walk(v.Evaluate, ir.ChildPath(parent, "gate", i, "evaluate"), p, idx, ld, moduleID)
		case *ir.Map:
			walk(v.Body, ir.ChildPath(parent, "map", i, "body"), p, idx, ld, moduleID)
		case *ir.Compose:
			walk(v.Body, ir.ChildPath(parent, "compose", i, "body"), p, idx, ld, moduleID)
		case *ir.CallStep:
			child, ok := callTargetModule(ld, moduleID, v.Call)
			if ok && child != nil && child.Workflow != nil {
				childParent := ir.CallWorkflowParentPath(path)
				walk(child.Workflow.Graph, childParent, p, stepPathIndex(child.Workflow.Graph, childParent), ld, child.ID)
			}
		}
	}
}

// staticPath returns the node's static IR path. Skip is handled by the caller (skipped
// before this is reached); the default arm is the sum-type exhaustiveness guard.
func staticPath(parent string, n ir.Node, i int) string {
	switch v := n.(type) {
	case *ir.CodeStep:
		return ir.PathFor(parent, "", v.ID, i)
	case *ir.AgentStep:
		return ir.PathFor(parent, "", v.ID, i)
	case *ir.SignalStep:
		return ir.PathFor(parent, "", v.ID, i)
	case *ir.CallStep:
		return ir.PathFor(parent, "", v.ID, i)
	case *ir.If:
		return ir.PathFor(parent, "if", "", i)
	case *ir.Loop:
		return ir.PathFor(parent, "loop", "", i)
	case *ir.Try:
		return ir.PathFor(parent, "try", "", i)
	case *ir.Parallel:
		return ir.PathFor(parent, "parallel", "", i)
	case *ir.Gate:
		return ir.PathFor(parent, "gate", "", i)
	case *ir.Map:
		return ir.PathFor(parent, "map", "", i)
	case *ir.Compose:
		return ir.PathFor(parent, "compose", "", i)
	default:
		panic(fmt.Sprintf("graph.staticPath: unexpected node type %T", n))
	}
}

// kindOf is the single mapping from ir node type to the projection's kind string. It is
// exhaustive over ir.Node's sealed sum type; a new node kind added without updating this
// switch panics loudly (guarded by TestKindOfExhaustive), rather than being silently
// mislabeled.
func kindOf(n ir.Node) string {
	switch n.(type) {
	case *ir.CodeStep:
		return "code"
	case *ir.AgentStep:
		return "agent"
	case *ir.SignalStep:
		return "signal"
	case *ir.CallStep:
		return "call"
	case *ir.If:
		return "if"
	case *ir.Loop:
		return "loop"
	case *ir.Try:
		return "try"
	case *ir.Parallel:
		return "parallel"
	case *ir.Gate:
		return "gate"
	case *ir.Skip:
		return "skip"
	case *ir.Map:
		return "map"
	case *ir.Compose:
		return "compose"
	default:
		panic(fmt.Sprintf("graph.kindOf: unexpected node type %T", n))
	}
}

// stepID returns a step node's id, or "" for control nodes.
func stepID(n ir.Node) string {
	switch v := n.(type) {
	case *ir.CodeStep:
		return v.ID
	case *ir.AgentStep:
		return v.ID
	case *ir.SignalStep:
		return v.ID
	case *ir.CallStep:
		return v.ID
	default:
		return ""
	}
}

// withOf returns an agent step's opaque `with:` config, or nil for every other kind.
func withOf(n ir.Node) ir.RawConfig {
	if a, ok := n.(*ir.AgentStep); ok {
		return a.With
	}
	return nil
}

func callTargetModule(ld *ir.LoadedDefinition, parentID, importID string) (*ir.LoadedModule, bool) {
	if ld == nil {
		return nil, false
	}
	for _, edge := range ld.ImportEdges {
		if edge.ParentID == parentID && edge.ImportID == importID {
			return ld.Module(edge.ChildID)
		}
	}
	return nil, false
}
