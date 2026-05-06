package obs

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// Project folds an event log into a deterministic span tree. It is a pure
// function: the same events yield byte-identical []Span (spec D6). Step spans
// come from node.started → node.completed/node.failed (two-event model); a
// step started but never finalized is Pending; node.skipped yields a "skipped"
// marker span. Control-scope spans and the run root are synthesized in later
// passes (Tasks 10-12).
//
// The blobs param is UNUSED in slice 6.1 (named `_`): agent.event *content*
// projection as span events is opt-in content-capture, deferred to 6.2's
// `awf trace --capture-content`. 6.1 reads only event timestamps + metadata.
func Project(events []state.Event, _ state.Blobs) ([]Span, error) {
	// lastTS is the "as-of" bound for Pending spans (deterministic, not Now()).
	var lastTS time.Time
	for _, e := range events {
		if e.TS.After(lastTS) {
			lastTS = e.TS
		}
	}

	// startSeen/finalized are LOCAL bookkeeping — they never leak into the
	// public Span (span.go keeps Span clean). On a resumed log a node may carry
	// two node.started events (epoch-1 crash + epoch-2 re-run); LAST-WINS — the
	// final node.started sets Start and the matching terminal event sets End, so
	// the span reflects the committed execution, not the abandoned one.
	byPath := map[string]*Span{}
	started := map[string]bool{}
	finalized := map[string]bool{}

	for _, e := range events {
		switch e.Type {
		case engine.EventNodeStarted:
			var d engine.NodeStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: node.started at %q: %w", e.Path, err)
			}
			s := ensureSpan(byPath, e.Path)
			s.Kind = d.Kind
			s.Start = e.TS // last-wins on resume
			started[e.Path] = true

		case engine.EventNodeCompleted:
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: node.completed at %q: %w", e.Path, err)
			}
			s := ensureSpan(byPath, e.Path)
			kind := s.Kind
			if kind == "" {
				kind = kindFromCompleted(d) // legacy log: no node.started
				s.Kind = kind
			}
			s.End = e.TS
			if !started[e.Path] {
				s.Start = e.TS // legacy / completion-only
			}
			s.Status = StatusOK
			finalized[e.Path] = true
			for k, v := range stepAttributes(e.Path, kind, d) {
				s.Attributes[k] = v
			}

		case engine.EventNodeFailed:
			var d engine.NodeFailedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: node.failed at %q: %w", e.Path, err)
			}
			s := ensureSpan(byPath, e.Path)
			s.End = e.TS
			if !started[e.Path] {
				s.Start = e.TS
			}
			s.Status = StatusError
			s.StatusMsg = d.Error
			finalized[e.Path] = true
			s.Attributes[AttrNodePath] = e.Path
			s.Attributes[AttrNodeOutcome] = d.Outcome
			// R3: carry the required awf.node.kind on failed spans too. s.Kind is
			// set when a node.started preceded (the common post-start failure). A
			// PRE-start failure (e.g. a with:-substitution error before dispatch)
			// has no node.started → kind is genuinely unknown to obs; left unset
			// rather than fabricated. (Full coverage would add Kind to
			// NodeFailedData — deferred unless 6.3 conformance demands it.)
			if s.Kind != "" {
				s.Attributes[AttrNodeKind] = s.Kind
			}

		case engine.EventNodeSkipped:
			// node.skipped is emitted for a `skip` node (skip.go:65); it is NOT a
			// dispatched step so it has no node.started. Project it as a short
			// "skipped" leaf span (skip is one of the 10 real node kinds).
			var d engine.NodeSkippedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: node.skipped at %q: %w", e.Path, err)
			}
			s := ensureSpan(byPath, e.Path)
			if s.Kind == "" {
				s.Kind = "skip"
			}
			if s.Start.IsZero() {
				s.Start = e.TS
			}
			s.End = e.TS
			s.Status = StatusOK
			finalized[e.Path] = true
			s.Attributes[AttrNodePath] = e.Path
			s.Attributes[AttrNodeKind] = s.Kind
			s.Attributes[AttrNodeOutcome] = "skipped"
			if d.Reason != "" {
				s.Attributes["awf.skip.reason"] = d.Reason
			}
		}
	}

	// Finalize Pending spans (started, never finalized).
	for path, s := range byPath {
		if !finalized[path] {
			s.Pending = true
			s.End = lastTS
			s.Attributes[AttrNodePath] = path
			if s.Kind != "" {
				s.Attributes[AttrNodeKind] = s.Kind
			}
			if s.Attributes[AttrNodeOutcome] == nil {
				s.Attributes[AttrNodeOutcome] = outcomeIncomplete
			}
		}
	}

	// Synthesize control-scope ancestor spans by walking ParentPath from every
	// leaf. A scope's bounds enclose all descendants (min start, max end). This
	// makes the span tree mirror the full engine/path addressing tree (spec D2).
	synthesizeScopes(byPath)

	if err := attachControlEvents(byPath, events); err != nil {
		return nil, err
	}

	return collect(byPath), nil
}

// ensureSpan returns the span for a path, creating it with an initialized
// attribute map, ParentPath, and a LOW-CARDINALITY Name on first touch. For a
// leaf step the Name is its leaf path segment (the step id, index-free, e.g.
// "scan" from "loop[0].body.iter-3.scan") — the OTel span-name rule (R8); the
// full unique path stays in awf.node.path. Scopes set Name=kind in synthesizeScopes.
func ensureSpan(byPath map[string]*Span, path string) *Span {
	s, ok := byPath[path]
	if !ok {
		parent, hasParent := engine.ParentPath(path)
		name := path
		if hasParent {
			name = path[len(parent)+1:] // leaf segment = step id
		}
		s = &Span{Path: path, ParentPath: parent, Name: name, Attributes: map[string]any{}}
		byPath[path] = s
	}
	return s
}

// kindFromCompleted infers a leaf step kind for a legacy log (no node.started).
// ExitCode present ⇒ code step; otherwise agent/signal (indistinguishable from
// the completed payload alone — "agent" is the safe label since signal steps
// carry no metrics and render identically).
func kindFromCompleted(d engine.NodeCompletedData) string {
	if d.ExitCode != nil {
		return "code"
	}
	return "agent"
}

// collect returns the spans sorted by Path (deterministic order).
func collect(byPath map[string]*Span) []Span {
	out := make([]Span, 0, len(byPath))
	for _, s := range byPath {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

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
	case hasPrefix(seg, "iter-"):
		return "iteration"
	case hasPrefix(seg, "attempt-"):
		return "attempt"
	case hasPrefix(seg, "item-"):
		return "item"
	case hasPrefix(seg, "gate["):
		return "gate"
	case hasPrefix(seg, "loop["):
		return "loop"
	case hasPrefix(seg, "if["):
		return "if"
	case hasPrefix(seg, "try["):
		return "try"
	case hasPrefix(seg, "parallel["):
		return "parallel"
	case hasPrefix(seg, "map["):
		return "map"
	default:
		return "scope"
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// attachControlEvents folds the control-flow marker events (branch.taken,
// loop.iter, gate.attempt) onto their (already-synthesized) scope spans as
// attributes / span events. Returns an error on a malformed payload —
// consistent with Project's main loop (a corrupt log is a real fault, surfaced
// loudly, not silently dropped, M2). Each gate.attempt also emits a
// gen_ai.evaluation.result event (spec D7) parented to the gate span — awf.*
// stays canonical; the event is OTel-interop sugar.
//
// R6: no map.item enrichment. The awf.map.items tally was a plan-only
// invention (absent from standard §9, no consumer); per-item fan-out is
// recoverable as the count of map[i].item-N child scope spans, which
// synthesizeScopes already creates from the item steps' events.
func attachControlEvents(byPath map[string]*Span, events []state.Event) error {
	for _, e := range events {
		switch e.Type {
		case engine.EventBranchTaken:
			var d engine.BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("obs.Project: branch.taken at %q: %w", e.Path, err)
			}
			if s, ok := byPath[e.Path]; ok {
				s.Attributes["awf.branch"] = d.Which
			}
		case engine.EventLoopIter:
			var d engine.LoopIterData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("obs.Project: loop.iter at %q: %w", e.Path, err)
			}
			if s, ok := byPath[e.Path]; ok {
				s.Attributes["awf.loop.iterations"] = int64(d.N)
			}
		case engine.EventGateAttempt:
			var d engine.GateAttemptData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("obs.Project: gate.attempt at %q: %w", e.Path, err)
			}
			if s, ok := byPath[e.Path]; ok {
				s.Attributes[AttrGateAttempts] = int64(d.N)
				s.Attributes[AttrGateOutcome] = gateOutcomeString(d.AttemptOutcome)
				s.Events = append(s.Events, SpanEvent{
					Name: EventGenAIEvaluation,
					Time: e.TS,
					Attributes: map[string]any{
						AttrGenAIEvalName:  "until",
						"awf.gate.attempt": int64(d.N),
						"awf.gate.status":  gateOutcomeString(d.AttemptOutcome),
					},
				})
			}
		}
	}
	return nil
}

// gateOutcomeString maps the engine gate-attempt outcome to the awf.gate.outcome
// attribute value. The gate-span outcome is the LAST attempt's outcome (events
// process in log order, so the final write wins).
func gateOutcomeString(outcome string) string {
	switch outcome {
	case engine.AttemptPassed: // "attempt_passed"
		return "passed"
	case engine.AttemptRejected: // "attempt_rejected"
		return "rejected"
	default:
		return outcome
	}
}
