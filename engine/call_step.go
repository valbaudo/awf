package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

func runCallStep(ctx context.Context, call *ir.CallStep, path string, ictx interpreterContext) (Outcome, error) {
	if _, done := ictx.runstate.LookupCompleted(path); done {
		return OutcomeOK, nil
	}
	appendNodeStarted(ictx.log, path, "call")

	child, ok := callTargetModule(ictx.def, ictx.moduleID, call.Call)
	if !ok || child == nil || child.Workflow == nil {
		return failStep(ictx.log, path, OutcomePermanentFailure, fmt.Errorf("call target %q not found from module %q", call.Call, ictx.moduleID))
	}
	ld, ok := ictx.dispatcher.(*LocalDispatcher)
	if !ok {
		return "", fmt.Errorf("engine.runCallStep: call at %q requires *LocalDispatcher (got %T)", path, ictx.dispatcher)
	}

	runtimeParent := CallWorkflowRuntimePath(path)
	childHandles, err := createRuntimeHandles(ctx, ld, child.Workflow, child.ComposeFiles, runtimeParent, ictx.runstate)
	if err != nil {
		return failStep(ictx.log, path, OutcomeRetryableFailure, err)
	}
	defer func() {
		_ = destroyRuntimeHandles(ld.Backend, childHandles)
	}()

	childDispatcher := cloneDispatcherForRuntime(ld, runtimeParent, child.ComposeFiles, childHandles)
	var callInput map[string]any
	var inputRef string
	var runtimes []ResolvedRuntime
	if rec, recorded := ictx.runstate.LookupCallStarted(path); recorded {
		callInput = rec.Input
		inputRef = rec.InputRef
		runtimes = rec.Runtimes
		current, rerr := ResolveRuntimes(ctx, WalkRuntimeRefs(child.ID, runtimeParent, child.Workflow), childDispatcher.Resolver, childDispatcher.Handles)
		if rerr != nil {
			return "", fmt.Errorf("engine.runCallStep: resolve current runtimes at %q: %w", path, rerr)
		}
		if derr := CheckRuntimesDrift(runtimes, current); derr != nil {
			return "", derr
		}
	} else {
		callInput, err = evaluateCallInput(call, path, child.Workflow, ictx)
		if err != nil {
			return failStep(ictx.log, path, OutcomePermanentFailure, err)
		}
		raw, merr := json.Marshal(callInput)
		if merr != nil {
			return "", fmt.Errorf("engine.runCallStep: marshal input at %q: %w", path, merr)
		}
		inputRef, err = ictx.blobs.Put(raw)
		if err != nil {
			return "", fmt.Errorf("engine.runCallStep: put input at %q: %w", path, err)
		}
		runtimes, err = ResolveRuntimes(ctx, WalkRuntimeRefs(child.ID, runtimeParent, child.Workflow), childDispatcher.Resolver, childDispatcher.Handles)
		if err != nil {
			return failStep(ictx.log, path, OutcomePermanentFailure, err)
		}
		if err := appendCallStarted(ictx.log, path, inputRef, runtimes); err != nil {
			return "", err
		}
		ictx.runstate.RecordCallStarted(path, CallStartedRecord{Input: callInput, InputRef: inputRef, Runtimes: runtimes})
	}
	_ = inputRef

	childCtx := ictx
	childCtx.moduleID = child.ID
	childCtx.wf = child.Workflow
	childCtx.input = callInput
	childCtx.runtimeParent = runtimeParent
	childCtx.dispatcher = childDispatcher
	oc, childErr := interpNodes(ctx, child.Workflow.Graph, runtimeParent, childCtx)
	if childErr != nil || oc != OutcomeOK {
		if oc == "" {
			return "", childErr
		}
		childPath := lastFailedChildPath(ictx.log, runtimeParent)
		if childPath == "" {
			childPath = runtimeParent
		}
		boundaryOutcome := oc
		cause := childErr
		if cause == nil {
			cause = fmt.Errorf("child workflow returned outcome %q", oc)
		}
		return failStep(ictx.log, path, boundaryOutcome, fmt.Errorf("child workflow failed at %s: %w", childPath, cause))
	}

	product, err := evaluateWorkflowExports(ictx.runstate, child.Workflow, path, callInput, ictx.blobs)
	if err != nil {
		return failStep(ictx.log, path, OutcomePermanentFailure, err)
	}
	nr, err := commitCallProduct(ictx.log, ictx.blobs, path, product)
	if err != nil {
		return "", err
	}
	ictx.runstate.RecordCompleted(path, nr)
	return OutcomeOK, nil
}

func evaluateCallInput(call *ir.CallStep, path string, child *ir.Workflow, ictx interpreterContext) (map[string]any, error) {
	out := map[string]any{}
	scope := ictx.scope(path)
	for key, raw := range call.Input {
		value, err := template.EvalTemplateValue(raw, scope)
		if err != nil {
			return nil, fmt.Errorf("call input %q: %w", key, err)
		}
		out[key] = value
	}
	if child.Input == nil {
		if len(out) > 0 {
			return nil, fmt.Errorf("call input provided but child workflow declares no input schema")
		}
		return out, nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal call input: %w", err)
	}
	validated, err := ValidateAgainstSchema(raw, child.Input)
	if err != nil {
		return nil, fmt.Errorf("call input schema validation: %w", err)
	}
	return validated, nil
}

func appendCallStarted(log state.Log, path, inputRef string, runtimes []ResolvedRuntime) error {
	data, err := json.Marshal(CallStartedData{InputRef: inputRef, Runtimes: runtimes})
	if err != nil {
		return fmt.Errorf("engine.runCallStep: marshal call.started at %q: %w", path, err)
	}
	if err := log.Append(state.Event{Type: EventCallStarted, Path: path, Data: data}); err != nil {
		return fmt.Errorf("engine.runCallStep: append call.started at %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return fmt.Errorf("engine.runCallStep: sync after call.started at %q: %w", path, err)
	}
	return nil
}

func callTargetModule(ld *ir.LoadedDefinition, parentID, importID string) (*ir.LoadedModule, bool) {
	if ld == nil {
		return nil, false
	}
	for _, edge := range ld.ImportEdges {
		if edge.ParentID == parentID && edge.ImportID == importID {
			return ld.Module(edge.ChildID)
		}
	}
	return nil, false
}

func lastFailedChildPath(log state.Log, childParent string) string {
	events, err := log.Fold()
	if err != nil {
		return ""
	}
	prefix := childParent + "."
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventNodeFailed && (events[i].Path == childParent || strings.HasPrefix(events[i].Path, prefix)) {
			return events[i].Path
		}
	}
	return ""
}
