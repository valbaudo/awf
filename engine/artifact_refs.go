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
	if cas, handled, err := resolveCallArtifactRef(scope, wf, id, name); handled || err != nil {
		return cas, err
	}
	idx := ir.OutputFilesByStepID(wf)
	declaredPath, ok := idx[id].PathForName(name)
	if !ok {
		return "", fmt.Errorf("step %q has no named output_files artifact %q", id, name)
	}
	return scope.ResolveDeclaredArtifactPath(id, declaredPath)
}

func resolveCallArtifactRef(scope *Scope, wf *ir.Workflow, id, name string) (string, bool, error) {
	if scope == nil {
		return "", false, nil
	}
	if wf == nil {
		wf = scope.wfRef
	}
	staticPath, ok := callStepPathIndex(wf)[id]
	if !ok {
		return "", false, nil
	}
	runtimePath, err := scope.stepRuntimePath(staticPath)
	if err != nil {
		return "", true, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: err.Error()}
	}
	nr, ok := scope.rs.LookupCompleted(runtimePath)
	if !ok {
		return "", true, template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: call step %q not yet committed (%s)", id, runtimePath)
	}
	cas, ok := nr.Files[name]
	if !ok {
		return "", true, template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: call step %q has no exported artifact %q", id, name)
	}
	return cas, true, nil
}

func callStepPathIndex(wf *ir.Workflow) map[string]string {
	out := map[string]string{}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		if call, ok := n.(*ir.CallStep); ok {
			out[call.ID] = path
		}
	})
	return out
}

// callChildOutputSchemas maps each call step id in wf to the OUTPUT_SCHEMA of the
// child workflow it invokes (value nil when the child declares none). Resolves
// each call target through def's import edges from the evaluating moduleID — the
// same lookup runCallStep uses. def==nil (no LoadedDefinition available) yields a
// nil map, which callFieldDeclaredButAbsent treats as "no cross-call classification"
// (historic AWF4002 behavior). Part C C6.
func callChildOutputSchemas(def *ir.LoadedDefinition, moduleID string, wf *ir.Workflow) map[string]*ir.JSONSchema {
	if def == nil || wf == nil {
		return nil
	}
	out := map[string]*ir.JSONSchema{}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, _ string) {
		call, ok := n.(*ir.CallStep)
		if !ok {
			return
		}
		child, ok := callTargetModule(def, moduleID, call.Call)
		if !ok || child == nil || child.Workflow == nil {
			return
		}
		out[call.ID] = child.Workflow.OutputSchema
	})
	return out
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
