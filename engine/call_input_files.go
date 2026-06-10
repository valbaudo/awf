package engine

import (
	"fmt"
	"sort"

	"github.com/valbaudo/awf/ir"
)

func resolveCallInputFiles(call *ir.CallStep, child *ir.LoadedModule, path string, ictx interpreterContext) (map[string]string, error) {
	childInputFiles := ir.WorkflowInputFiles(nil)
	if child != nil && child.Workflow != nil {
		childInputFiles = child.Workflow.InputFiles
	}
	callInputFiles := call.InputFiles
	childNames := make([]string, 0, len(childInputFiles))
	for name := range childInputFiles {
		childNames = append(childNames, name)
	}
	sort.Strings(childNames)
	for _, name := range childNames {
		if _, ok := callInputFiles[name]; !ok {
			return nil, fmt.Errorf("call input_files.%s: child workflow requires input file %q", name, name)
		}
	}
	if len(callInputFiles) == 0 {
		return nil, nil
	}

	scope := ictx.scope(path)
	names := make([]string, 0, len(callInputFiles))
	for name := range callInputFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(callInputFiles))
	for _, name := range names {
		rawRef := callInputFiles[name]
		contract, ok := childInputFiles[name]
		if !ok {
			return nil, fmt.Errorf("call input_files.%s: child workflow does not declare input file %q", name, name)
		}
		ref, content, err := resolveSingleInputFileRef(rawRef, scope, ictx.wf, ictx.moduleID, ictx.blobs, ictx.runstate.Assets)
		if err != nil {
			return nil, fmt.Errorf("call input_files.%s: %w", name, err)
		}
		resolvedContract, hasContract, err := resolveArtifactContract("input_files."+name, contract, child.ID, ictx.runstate.Assets, ictx.blobs)
		if err != nil {
			return nil, fmt.Errorf("call input_files.%s: %w", name, err)
		}
		if hasContract {
			if err := ValidateArtifactContract("input_files."+name, content, resolvedContract); err != nil {
				return nil, fmt.Errorf("call input_files.%s: %w", name, err)
			}
		}
		out[name] = ref
	}
	return out, nil
}
