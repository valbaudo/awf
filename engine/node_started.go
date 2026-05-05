package engine

import (
	"encoding/json"

	"github.com/valbaudo/awf/state"
)

// appendNodeStarted writes an observational node.started event for a step node
// entering dispatch. Best-effort: node.started is non-durability-critical (it
// rides the next fsync, like agent.event), so an append error here does NOT
// fail the step — a genuinely broken log surfaces at the imminent
// node.completed commit. Callers invoke this AFTER their resume short-circuit
// so a replayed (already-committed) node never re-emits a start.
//
// Per CLAUDE.md "interpreter is the only writer to state": this is called from
// the interpreter-side step handlers (runCodeStep / runAgentStep /
// runSignalStep), never from the dispatcher.
func appendNodeStarted(log state.Log, path, kind string) {
	data, err := json.Marshal(NodeStartedData{Kind: kind})
	if err != nil {
		return // unreachable for a fixed struct; observational — never block dispatch
	}
	_ = log.Append(state.Event{Type: EventNodeStarted, Path: path, Data: data})
}
