package graph

import (
	"encoding/json"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
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
	attachLivePreviews(m, events)
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

func attachLivePreviews(m map[string]NodeState, events []state.Event) {
	for _, e := range events {
		if e.Type != engine.EventAgentEvent {
			continue
		}
		var d engine.AgentEventData
		if err := json.Unmarshal(e.Data, &d); err != nil || !d.Live {
			continue
		}
		st := m[e.Path]
		preview := ""
		if d.DisplaySummary != "" {
			preview = agent.SanitizeDisplayText(d.DisplaySummary)
		} else if d.PayloadInline != nil {
			preview = agent.SanitizeDisplayBytes(d.PayloadInline)
		}
		preview = agent.RedactDisplayText(preview)
		st.LivePreview = preview
		st.LiveDisplayClass = d.DisplayClass
		st.LiveDisplayTool = agent.RedactDisplayText(agent.SanitizeDisplayText(d.DisplayTool))
		st.LiveDisplayLines = d.DisplayLines
		st.LiveDisplayBytes = d.DisplayBytes
		st.LiveDisplayIsError = d.DisplayIsError
		m[e.Path] = st
	}
}
