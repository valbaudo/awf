package engine

import (
	"fmt"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

func resolveNamedArtifactRef(scope *Scope, wf *ir.Workflow, id, name string) (string, error) {
	if cas, handled, err := resolveReducedBodyArtifactRef(scope, wf, id, name); handled || err != nil {
		return cas, err
	}
	idx := ir.OutputFilesByStepID(wf)
	declaredPath, ok := idx[id].PathForName(name)
	if !ok {
		return "", fmt.Errorf("step %q has no named output_files artifact %q", id, name)
	}
	return scope.ResolveDeclaredArtifactPath(id, declaredPath)
}

func resolveReducedBodyArtifactRef(scope *Scope, wf *ir.Workflow, id, name string) (string, bool, error) {
	if scope == nil {
		return "", false, nil
	}
	if wf == nil {
		wf = scope.wfRef
	}
	staticPath, ok := scope.stepIndex[id]
	if !ok {
		return "", false, nil
	}
	mapPath, _, ok := ir.SingleMapBodyShape(staticPath)
	if !ok {
		return "", false, nil
	}
	_, inside, err := scope.instanceFromCtx(mapPath, itemSep)
	if err != nil {
		return "", true, err
	}
	if inside {
		return "", false, nil
	}
	m := mapPathIndex(wf)[mapPath]
	if m == nil || m.Reduce == nil {
		return "", false, nil
	}
	declaredPath, ok := m.Reduce.OutputFiles.PathForName(name)
	if !ok {
		return "", true, fmt.Errorf("reduced map (producer %q) reducer has no named output_files artifact %q", id, name)
	}
	if wf == nil {
		return "", true, template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: reduced map %q cannot resolve declared path without workflow", id)
	}
	containerPath, err := template.Substitute(declaredPath, newReduceTemplateScope(scope.rs, wf, mapPath))
	if err != nil {
		return "", true, fmt.Errorf("substitute reduced map artifact path %q: %w", declaredPath, err)
	}
	cas, err := scope.ResolveArtifactPath(id, containerPath)
	return cas, true, err
}
