package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

func commitCallProduct(log state.Log, blobs state.Blobs, path string, result WorkflowExportResult) (NodeResult, error) {
	for name, ref := range result.Files {
		if _, err := blobs.Get(ref); err != nil {
			return NodeResult{}, fmt.Errorf("call export file %q ref %q is missing from blobs: %w", name, ref, err)
		}
	}
	nr := NodeResult{Outcome: OutcomeOK, Outputs: result.Outputs, Files: result.Files}
	if result.Outputs != nil {
		outBytes, err := json.Marshal(result.Outputs)
		if err != nil {
			return NodeResult{}, fmt.Errorf("commit call product %q: marshal outputs: %w", path, err)
		}
		ref, err := blobs.Put(outBytes)
		if err != nil {
			return NodeResult{}, fmt.Errorf("commit call product %q: put outputs: %w", path, err)
		}
		nr.OutputsRef = ref
	}
	if err := appendNodeCompleted(log, path, NodeCompletedData{
		Outcome:    string(OutcomeOK),
		OutputsRef: nr.OutputsRef,
		Files:      result.Files,
	}); err != nil {
		return NodeResult{}, fmt.Errorf("commit call product %q: %w", path, err)
	}
	return nr, nil
}
