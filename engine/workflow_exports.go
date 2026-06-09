package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

type WorkflowExportResult struct {
	Outputs map[string]any
	Files   map[string]string
}

func evaluateWorkflowExports(parent *RunState, wf *ir.Workflow, callPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	child := childRunStateForCall(parent, callPath, input)
	scope := NewScopeWithInput(child, wf, ir.CallWorkflowParentPath(callPath), input)

	var out WorkflowExportResult
	if len(wf.Outputs) > 0 {
		out.Outputs = make(map[string]any, len(wf.Outputs))
		keys := make([]string, 0, len(wf.Outputs))
		for key := range wf.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := template.EvalTemplateValue(wf.Outputs[key], scope)
			if err != nil {
				return WorkflowExportResult{}, fmt.Errorf("evaluate workflow output %q: %w", key, err)
			}
			out.Outputs[key] = value
		}
		if wf.OutputSchema != nil {
			if err := ValidateOutputMap(out.Outputs, wf.OutputSchema); err != nil {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_schema validation: %w", err)
			}
		}
	}

	if len(wf.ArtifactExports) > 0 {
		out.Files = make(map[string]string, len(wf.ArtifactExports))
		keys := make([]string, 0, len(wf.ArtifactExports))
		for key := range wf.ArtifactExports {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			raw := wf.ArtifactExports[key]
			if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s must be a static step.<id>.files.<name> reference, not a template", key)
			}
			id, name, ok := template.ParseArtifactRef(raw)
			if !ok {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s=%s: expected step.<id>.files.<name>", key, raw)
			}
			ref, err := resolveNamedArtifactRef(scope, wf, id, name)
			if err != nil {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s: %w", key, err)
			}
			if blobs != nil {
				if _, err := blobs.Get(ref); err != nil {
					return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s ref %q is missing from blobs: %w", key, ref, err)
				}
			}
			out.Files[key] = ref
		}
	}

	return out, nil
}

func childRunStateForCall(parent *RunState, callPath string, input map[string]any) *RunState {
	child := NewRunState(parent.RunID, parent.WorkflowDigest, input)
	prefix := ir.CallWorkflowParentPath(callPath)
	parent.mu.Lock()
	defer parent.mu.Unlock()
	copyPrefixedCompleted(child.Completed, parent.Completed, prefix)
	copyPrefixedBranches(child.Branches, parent.Branches, prefix)
	copyPrefixedLoopIters(child.LoopIters, parent.LoopIters, prefix)
	copyPrefixedGateAttempts(child.GateAttempts, parent.GateAttempts, prefix)
	copyPrefixedMapItems(child.MapItems, parent.MapItems, prefix)
	return child
}

func copyPrefixedCompleted(dst, src map[string]NodeResult, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedBranches(dst, src map[string]string, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedLoopIters(dst, src map[string]int, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedGateAttempts(dst, src map[string][]AttemptResult, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			cp := make([]AttemptResult, len(value))
			copy(cp, value)
			dst[childPath] = cp
		}
	}
}

func copyPrefixedMapItems(dst, src map[string][]MapItemRecord, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			cp := make([]MapItemRecord, len(value))
			copy(cp, value)
			dst[childPath] = cp
		}
	}
}

func stripChildPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "", true
	}
	prefix += "."
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}
