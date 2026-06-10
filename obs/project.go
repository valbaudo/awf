package obs

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// ProjectOptions tunes the projection. CaptureContent (default false) makes
// ProjectWithOptions dereference agent I/O + typed-output/stdout blobs and
// attach them to spans (slice 6.2, awf trace --capture-content); when false the
// blobs param is unused and the output is byte-identical to slice 6.1 (so the
// conformance byte-identical-replay assertion holds).
type ProjectOptions struct {
	CaptureContent bool
}

// Project is the default projection (no content capture). Preserved signature
// from slice 6.1; a thin wrapper over ProjectWithOptions.
func Project(events []state.Event, blobs state.Blobs) ([]Span, error) {
	return ProjectWithOptions(events, blobs, ProjectOptions{})
}

// ProjectWithOptions folds an event log into a deterministic span tree (see
// Project's slice-6.1 contract). With opts.CaptureContent it additionally
// attaches raw agent I/O (agent.event payloads, inline or via blobs) and the
// typed-output / stdout blobs as span events / attributes under awf.* — opaque,
// never parsed (the "obs must not parse harness internals" invariant). Requires
// a non-nil blobs when CaptureContent is set.
func ProjectWithOptions(events []state.Event, blobs state.Blobs, opts ProjectOptions) ([]Span, error) {
	if opts.CaptureContent && blobs == nil {
		return nil, fmt.Errorf("obs.ProjectWithOptions: CaptureContent requires a non-nil blob store")
	}
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

	// Run-metadata capture: populated from run.started / run.resumed /
	// run.finished / run.cancelled. Used by buildRootSpan (Task 12).
	var runStarted *engine.RunStartedData
	var runStartTS, runEndTS time.Time
	runFinalized := false
	var runOutcome string // run.finished.Outcome, or "cancelled" for run.cancelled (R2)
	var epoch uint32      // run.started ⇒ 1; each run.resumed ⇒ its payload epoch (mirrors engine/fold.go)

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

		case engine.EventCallStarted:
			var d engine.CallStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: call.started at %q: %w", e.Path, err)
			}
			s := ensureSpan(byPath, e.Path)
			s.Kind = "call"
			if !started[e.Path] {
				s.Start = e.TS
				started[e.Path] = true
			}
			attachCallStartedAttrs(s, e.Path, d)

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
			if opts.CaptureContent {
				attachNodeCompletedContent(s, d, blobs)
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

		case engine.EventRunStarted:
			var d engine.RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: run.started: %w", err)
			}
			runStarted = &d
			runStartTS = e.TS
			epoch = 1
		case engine.EventRunResumed:
			var d engine.RunResumedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: run.resumed: %w", err)
			}
			epoch = d.Epoch
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: run.finished: %w", err)
			}
			runEndTS = e.TS
			runFinalized = true
			runOutcome = d.Outcome
		case engine.EventRunCancelled:
			runEndTS = e.TS
			runFinalized = true
			runOutcome = "cancelled" // terminal → Error (R2)

		case engine.EventAgentEvent:
			if !opts.CaptureContent {
				continue
			}
			var d engine.AgentEventData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				// Malformed log STRUCTURE is a real fault → abort (M2 invariant).
				return nil, fmt.Errorf("obs.Project: agent.event at %q: %w", e.Path, err)
			}
			attachAgentEventContent(ensureSpan(byPath, e.Path), d, blobs, e.TS)

		case engine.EventSkillsSelected:
			var d engine.SkillsSelectedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("obs.Project: skills.selected at %q: %w", e.Path, err)
			}
			if e.Path == "" {
				return nil, fmt.Errorf("obs.Project: skills.selected has empty path")
			}
			s := ensureSpan(byPath, e.Path)
			if s.Start.IsZero() {
				s.Start = e.TS
			}
			attachSkillsSelectedEvent(s, d, e.TS)

		case engine.EventNodeSkipped:
			// node.skipped is emitted for a `skip` node (skip.go:65); it is NOT a
			// dispatched step so it has no node.started. Project it as a short
			// "skipped" leaf span (skip is one of the 12 real node kinds).
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
			s.Attributes[AttrNodeOutcome] = outcomeSkipped
			if d.Reason != "" {
				s.Attributes[AttrSkipReason] = d.Reason
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

	root := buildRootSpan(byPath, runStarted, runStartTS, runEndTS, runFinalized, lastTS, epoch, runOutcome)
	byPath[""] = root
	applyRunScopedAttrs(byPath, runStarted)

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
