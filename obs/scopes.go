package obs

import (
	"strings"
	"time"

	"github.com/valbaudo/awf/engine"
)

// synthesizeScopes interns every ancestor path as a control-scope span, then
// computes each scope's bounds with a single post-order fold over a one-pass
// children index — the Zipkin keyToNode/addChild + Jaeger spanMap pattern,
// adapted to path identity. O(N) in nodes; no fixed-point re-scan, no pairwise
// hasAncestor. Correctness rests on engine.ParentPath being segment-EXACT (the
// children-index key is the literal parent string, so "gate[0]" and "gate[01]"
// never collide — TestParentPath locks this).
func synthesizeScopes(byPath map[string]*Span) {
	// 1. Intern all ancestors. Snapshot the leaf paths first (we add to byPath).
	seed := make([]string, 0, len(byPath))
	for p := range byPath {
		seed = append(seed, p)
	}
	for _, p := range seed {
		for cur := p; ; {
			parent, ok := engine.ParentPath(cur)
			if !ok {
				break
			}
			if _, exists := byPath[parent]; !exists {
				sk := scopeKind(parent) // low-cardinality kind, used as both Name (R8) and awf.scope.kind
				s := &Span{Path: parent, Name: sk, Scope: true, Kind: sk, Attributes: map[string]any{}}
				if pp, hasPP := engine.ParentPath(parent); hasPP {
					s.ParentPath = pp
				}
				s.Attributes[AttrNodePath] = parent
				s.Attributes[AttrScopeKind] = sk // scopes carry awf.scope.kind, NOT awf.node.kind (M1)
				byPath[parent] = s
			}
			cur = parent
		}
	}
	// 2. One-pass children index keyed by exact parent path.
	children := map[string][]*Span{}
	for _, s := range byPath {
		if parent, ok := engine.ParentPath(s.Path); ok {
			children[parent] = append(children[parent], s)
		}
	}
	// 3. Post-order fold from every top-level root (forest: each node has one
	//    parent chain, so each is visited once). A scope's bounds enclose all
	//    descendants; Pending if any descendant is. Leaf bounds (from events)
	//    are never overwritten — only s.Scope spans recompute.
	var fold func(s *Span)
	fold = func(s *Span) {
		kids := children[s.Path]
		if len(kids) == 0 {
			return
		}
		var start, end time.Time
		anyPending := false
		for _, c := range kids {
			fold(c)
			if start.IsZero() || c.Start.Before(start) {
				start = c.Start
			}
			if c.End.After(end) {
				end = c.End
			}
			if c.Pending {
				anyPending = true
			}
		}
		if isScope(s) {
			s.Start, s.End = start, end
			// R2: structural scopes never claim Status=Ok and never roll up child
			// errors (non-idiomatic + would reintroduce map-order dependence).
			// They stay Unset; only an in-flight descendant surfaces (Pending).
			if anyPending {
				s.Pending = true
				if s.Attributes[AttrNodeOutcome] == nil {
					s.Attributes[AttrNodeOutcome] = outcomeIncomplete
				}
			}
		}
	}
	for _, s := range byPath {
		if _, hasParent := engine.ParentPath(s.Path); !hasParent {
			fold(s)
		}
	}
}

// isScope reports whether a span is a synthesized control scope vs a leaf step.
// Reads the explicit Span.Scope flag (set true at interning), decoupled from
// the Kind string so the bounds fold and the cost rollup stay correct as
// structural scope-kinds grow (M1).
func isScope(s *Span) bool { return s.Scope }

// scopeKind derives a control-scope span kind from a path's final segment.
func scopeKind(path string) string {
	seg := path
	if p, ok := engine.ParentPath(path); ok {
		seg = path[len(p)+1:]
	}
	switch {
	case seg == "then" || seg == "else":
		return "branch"
	case seg == "do" || seg == "catch" || seg == "finally":
		return "branch"
	case seg == "body":
		return "loop_body"
	case seg == "generate" || seg == "evaluate":
		return seg
	case strings.HasPrefix(seg, "iter-"):
		return "iteration"
	case strings.HasPrefix(seg, "attempt-"):
		return "attempt"
	case strings.HasPrefix(seg, "item-"):
		return "item"
	case strings.HasPrefix(seg, "gate["):
		return "gate"
	case strings.HasPrefix(seg, "loop["):
		return "loop"
	case strings.HasPrefix(seg, "if["):
		return "if"
	case strings.HasPrefix(seg, "try["):
		return "try"
	case strings.HasPrefix(seg, "parallel["):
		return "parallel"
	case strings.HasPrefix(seg, "map["):
		return "map"
	default:
		return "scope"
	}
}
