package graph

import (
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

// BuildWithRun returns the static graph PLUS the runtime expansion for one run: an
// instance node for every runtime path that has no static template node (map items, gate
// attempts, loop iterations, and their children), nested under its runtime parent, with
// run state attached as RunOverlay. Instance control edges are the static control edges
// projected into each instance context, so an item's body steps connect just like the
// template's do.
//
// Both the instance set and the overlay come from a single obs.Project of the event log
// (the source that carries running/failed/timing, which engine.Fold's RunState does not).
func BuildWithRun(wf *ir.Workflow, events []state.Event) (Projection, error) {
	return buildWithRunProjection(BuildStatic(wf), events)
}

// BuildWithRunLoaded returns the loaded static graph plus runtime instance nodes and
// overlay state. Imported child workflow template nodes are preserved under call sites,
// so callID.workflow.* runtime events overlay the corresponding template paths.
func BuildWithRunLoaded(ld *ir.LoadedDefinition, events []state.Event) (Projection, error) {
	return buildWithRunProjection(BuildStaticLoaded(ld), events)
}

func buildWithRunProjection(p Projection, events []state.Event) (Projection, error) {
	spans, err := obs.Project(events, nil)
	if err != nil {
		return Projection{}, err
	}

	static := make(map[string]bool, len(p.Nodes))
	for _, n := range p.Nodes {
		static[n.Path] = true
	}

	// Instance nodes: every non-root span that is not already a template node.
	overlay := make(map[string]NodeState, len(spans))
	var inst []Node
	for _, s := range spans {
		if s.Path == "" {
			continue // run root, not a node
		}
		overlay[s.Path] = stateFromSpan(s)
		if static[s.Path] {
			continue // a template node that ran; its state is in the overlay
		}
		inst = append(inst, Node{
			Path:       s.Path,
			Kind:       instanceKind(s),
			ID:         lastSegment(s.Path), // a leaf's step name ("work"), a scope's coordinate ("item-0")
			Parent:     s.ParentPath,
			NodeClass:  "instance",
			InstanceOf: nearestTemplateNode(templateOf(s.Path), static),
		})
	}
	attachLivePreviews(overlay, events)
	sort.Slice(inst, func(i, j int) bool { return inst[i].Path < inst[j].Path })

	allNodes := append(append([]Node{}, p.Nodes...), inst...)
	allEdges := append(append([]Edge{}, p.Edges...), instanceEdges(allNodes, p.Edges)...)

	// Declutter: a template step that has runtime instances is redundant in a run view —
	// the instances ARE its executions. Drop those template nodes (and any edge that loses
	// an endpoint). Containers (map/gate/loop nodes) and never-instanced templates stay, so
	// the run view shows actual executions nested under their scopes, not template stubs.
	// (The static graph — no --run — is unaffected; it still shows every template node.)
	instanced := map[string]bool{}
	for _, i := range inst {
		instanced[templateOf(i.Path)] = true
	}
	keep := map[string]bool{}
	nodes := make([]Node, 0, len(allNodes))
	for _, n := range allNodes {
		if n.NodeClass != "instance" && instanced[n.Path] {
			continue
		}
		nodes = append(nodes, n)
		keep[n.Path] = true
	}
	edges := make([]Edge, 0, len(allEdges))
	for _, e := range allEdges {
		if keep[e.From] && keep[e.To] {
			edges = append(edges, e)
		}
	}

	p.Nodes = nodes
	p.Edges = edges
	if len(overlay) > 0 {
		p.RunOverlay = overlay
	}
	return p, nil
}

// instanceEdges projects each template-level control edge into every instance context.
// Two nodes in the same instance context (same item / attempt / iter coordinates) whose
// template paths share a static control edge get a runtime control edge — so an item's
// body steps connect just like the template's. Deterministic: contexts sorted, edges in
// projection order.
func instanceEdges(nodes []Node, edges []Edge) []Edge {
	byKey := map[string]string{} // (instanceContext|templatePath) -> runtime path
	ctxSet := map[string]bool{}
	for _, n := range nodes {
		byKey[instanceContext(n.Path)+"|"+templateOf(n.Path)] = n.Path
		if c := instanceContext(n.Path); c != "" {
			ctxSet[c] = true
		}
	}
	ctxs := make([]string, 0, len(ctxSet))
	for c := range ctxSet {
		ctxs = append(ctxs, c)
	}
	sort.Strings(ctxs)

	var out []Edge
	for _, c := range ctxs {
		for _, e := range edges {
			// Only project template-level control edges (both endpoints context "").
			if e.Kind != "control" || instanceContext(e.From) != "" || instanceContext(e.To) != "" {
				continue
			}
			from, okF := byKey[c+"|"+e.From]
			to, okT := byKey[c+"|"+e.To]
			if okF && okT {
				out = append(out, Edge{From: from, To: to, Kind: "control"})
			}
		}
	}
	return out
}

// instanceKind labels an instance node. A scope boundary segment maps to a scope kind;
// otherwise the leaf step's real kind (or a generic scope) is used.
func instanceKind(s obs.Span) string {
	switch last := lastSegment(s.Path); {
	case strings.HasPrefix(last, "item-"):
		return "map_item"
	case strings.HasPrefix(last, "attempt-"):
		return "gate_attempt"
	case strings.HasPrefix(last, "iter-"):
		return "loop_iter"
	}
	if s.Kind != "" {
		return s.Kind
	}
	if s.Scope {
		return "scope"
	}
	return "step"
}

// templateOf maps a runtime path to its static template path: an "item-N" segment maps
// back to the map's "body" boundary it replaced; "attempt-M" / "iter-K" segments were
// appended at runtime and are dropped. A path with no instance segments is its own
// template.
func templateOf(path string) string {
	segs := strings.Split(path, ".")
	out := segs[:0:0]
	for _, s := range segs {
		switch {
		case strings.HasPrefix(s, "item-"):
			out = append(out, "body") // map runtime replaces body with item-N
		case strings.HasPrefix(s, "attempt-"), strings.HasPrefix(s, "iter-"):
			// appended at runtime (gate attempt / loop iter) — drop
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, ".")
}

// instanceContext is the instance coordinates of a path: its item / attempt / iter
// segments joined, "" for a template-level path.
func instanceContext(path string) string {
	var segs []string
	for _, s := range strings.Split(path, ".") {
		if strings.HasPrefix(s, "item-") || strings.HasPrefix(s, "attempt-") || strings.HasPrefix(s, "iter-") {
			segs = append(segs, s)
		}
	}
	return strings.Join(segs, ".")
}

// nearestTemplateNode resolves an instance's InstanceOf: the nearest ancestor of tmpl
// that is an actual template node. A leaf instance's templateOf is already a node (returns
// itself); a scope instance's templateOf is a branch path (e.g. "map[0].body") that walks
// up to the enclosing map/gate/loop node ("map[0]"). Returns "" if none matches.
func nearestTemplateNode(tmpl string, static map[string]bool) string {
	for tmpl != "" {
		if static[tmpl] {
			return tmpl
		}
		i := strings.LastIndex(tmpl, ".")
		if i < 0 {
			return ""
		}
		tmpl = tmpl[:i]
	}
	return ""
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}
