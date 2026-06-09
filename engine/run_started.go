package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

// RunStartedDataFromEvents decodes the first run.started payload without
// dereferencing any blob refs. It is for resume-side pins and manifests; Fold
// remains responsible for materializing committed runtime state.
func RunStartedDataFromEvents(events []state.Event) (RunStartedData, error) {
	if len(events) == 0 || events[0].Type != EventRunStarted {
		return RunStartedData{}, fmt.Errorf("engine: no run.started event in log (malformed)")
	}
	var d RunStartedData
	if err := json.Unmarshal(events[0].Data, &d); err != nil {
		return RunStartedData{}, fmt.Errorf("engine: unmarshal run.started: %w", err)
	}
	return d, nil
}
