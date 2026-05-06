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
