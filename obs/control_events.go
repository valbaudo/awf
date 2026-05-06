package obs

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

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
				s.Attributes[AttrBranch] = d.Which
			}
		case engine.EventLoopIter:
			var d engine.LoopIterData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("obs.Project: loop.iter at %q: %w", e.Path, err)
			}
			if s, ok := byPath[e.Path]; ok {
				s.Attributes[AttrLoopIterations] = int64(d.N)
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
						AttrGenAIEvalName: "until",
						AttrGateAttempt:   int64(d.N),
						AttrGateStatus:    gateOutcomeString(d.AttemptOutcome),
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
