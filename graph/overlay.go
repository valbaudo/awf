package graph

import (
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

// Overlay derives per-path run state from a run's event log, keyed by runtime path.
// It reuses obs.Project — which already folds the event stream into spans carrying
// pending/failed/timing/outcome — because engine.Fold's RunState holds only COMMITTED
// facts and cannot represent running or failed nodes. The run-root span (Path "") is
// excluded: the overlay is node state, not run state.
//
// obs.Project is computed from FoldFile output, which tolerates a torn tail, so Overlay
// works on a live or crashed run's log (running nodes appear with state "running").
func Overlay(events []state.Event) (map[string]NodeState, error) {
	spans, err := obs.Project(events, nil)
	if err != nil {
		return nil, err
	}
	m := make(map[string]NodeState, len(spans))
	for _, s := range spans {
		if s.Path == "" {
			continue // run root, not a node
		}
		m[s.Path] = stateFromSpan(s)
	}
	return m, nil
}

// stateFromSpan normalizes a span to a single run-state token. Precedence: a pending
// span (started, not finalized) is running; a span with an error status is failed; an
// explicit skipped outcome is skipped; otherwise completed. The raw awf.node.outcome
// attribute is carried through in Outcome when present.
func stateFromSpan(s obs.Span) NodeState {
	outcome, _ := s.Attributes[obs.AttrNodeOutcome].(string)
	state := "completed"
	switch {
	case s.Pending:
		state = "running"
	case s.Status == obs.StatusError:
		state = "failed"
	case outcome == "skipped":
		state = "skipped"
	}
	return NodeState{State: state, Outcome: outcome}
}
