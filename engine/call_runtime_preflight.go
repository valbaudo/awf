package engine

import (
	"context"
	"fmt"

	"github.com/valbaudo/awf/ir"
)

func preflightCallStartedRuntimes(ctx context.Context, ictx interpreterContext) error {
	paths := ictx.runstate.CallStartedPaths()
	if len(paths) == 0 {
		return nil
	}
	ld, ok := ictx.dispatcher.(*LocalDispatcher)
	if !ok {
		return fmt.Errorf("engine.Run: call runtime preflight requires *LocalDispatcher (got %T)", ictx.dispatcher)
	}
	checked := map[string]struct{}{}
	if err := preflightCallStartedRuntimesInWorkflow(ctx, ld, ictx.def, ictx.runstate, ictx.moduleID, ictx.wf, ictx.runtimeParent, checked); err != nil {
		return err
	}
	for _, path := range paths {
		if _, ok := checked[path]; !ok {
			return fmt.Errorf("engine.Run: call.started at %q no longer maps to a call step", path)
		}
	}
	return nil
}

func preflightCallStartedRuntimesInWorkflow(
	ctx context.Context,
	ld *LocalDispatcher,
	def *ir.LoadedDefinition,
	runstate *RunState,
	moduleID string,
	wf *ir.Workflow,
	parent string,
	checked map[string]struct{},
) error {
	var walkErr error
	ir.WalkNodes(wf.Graph, parent, func(n ir.Node, path string) {
		if walkErr != nil {
			return
		}
		call, ok := n.(*ir.CallStep)
		if !ok {
			return
		}
		child, ok := callTargetModule(def, moduleID, call.Call)
		if !ok || child == nil || child.Workflow == nil {
			if _, recorded := runstate.LookupCallStarted(path); recorded {
				walkErr = fmt.Errorf("engine.Run: call target %q not found from module %q during runtime preflight", call.Call, moduleID)
			}
			return
		}
		runtimeParent := CallWorkflowRuntimePath(path)
		if rec, recorded := runstate.LookupCallStarted(path); recorded {
			checked[path] = struct{}{}
			if err := preflightOneCallStartedRuntime(ctx, ld, child, runtimeParent, rec, runstate); err != nil {
				walkErr = err
				return
			}
		}
		walkErr = preflightCallStartedRuntimesInWorkflow(ctx, ld, def, runstate, child.ID, child.Workflow, runtimeParent, checked)
	})
	return walkErr
}

func preflightOneCallStartedRuntime(ctx context.Context, ld *LocalDispatcher, child *ir.LoadedModule, runtimeParent string, rec CallStartedRecord, runstate *RunState) error {
	refs := WalkRuntimeRefs(child.ID, runtimeParent, child.Workflow)
	if !runtimeRefsNeedContainers(refs) {
		current, err := ResolveRuntimes(ctx, refs, ld.Resolver, nil)
		if err != nil {
			return fmt.Errorf("engine.Run: resolve current call runtimes at %q: %w", runtimeParent, err)
		}
		return CheckRuntimesDrift(rec.Runtimes, current)
	}
	childHandles, err := createRuntimeHandles(ctx, ld, child.Workflow, child.ComposeFiles, runtimeParent, runstate)
	if err != nil {
		return fmt.Errorf("engine.Run: create call runtime handles at %q: %w", runtimeParent, err)
	}
	defer func() {
		_ = destroyRuntimeHandles(ld.Backend, childHandles)
	}()
	childDispatcher := cloneDispatcherForRuntime(ld, runtimeParent, child.ComposeFiles, childHandles)
	current, err := ResolveRuntimes(ctx, refs, childDispatcher.Resolver, childDispatcher.Handles)
	if err != nil {
		return fmt.Errorf("engine.Run: resolve current call runtimes at %q: %w", runtimeParent, err)
	}
	return CheckRuntimesDrift(rec.Runtimes, current)
}

func runtimeRefsNeedContainers(refs []RuntimeRef) bool {
	for _, ref := range refs {
		if ref.Container != "" {
			return true
		}
	}
	return false
}
