package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

// RunFinishedDataFromEvent decodes a run.finished event's payload. engine.Fold
// ignores run.finished (it is observational), so resume must read the terminal
// outcome from the raw matched event, not from RunState. The caller passes the
// matched event — run.finished is never events[0].
func RunFinishedDataFromEvent(e state.Event) (RunFinishedData, error) {
	var d RunFinishedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return RunFinishedData{}, fmt.Errorf("engine: unmarshal run.finished: %w", err)
	}
	return d, nil
}

// NodeFailedDataFromEvent decodes a node.failed event's payload (same rationale).
func NodeFailedDataFromEvent(e state.Event) (NodeFailedData, error) {
	var d NodeFailedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return NodeFailedData{}, fmt.Errorf("engine: unmarshal node.failed: %w", err)
	}
	return d, nil
}
