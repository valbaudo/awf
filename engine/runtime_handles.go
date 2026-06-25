package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func createRuntimeHandles(ctx context.Context, ld *LocalDispatcher, wf *ir.Workflow, composeFiles map[string][]byte, runtimeParent string, runstate *RunState) (map[string]container.Handle, error) {
	handles := make(map[string]container.Handle, len(wf.Containers))
	mapImageTargets := ir.MapImageTargets(wf)
	names := make([]string, 0, len(wf.Containers))
	for name := range wf.Containers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if mapImageTargets[name] {
			continue
		}
		handleKey := QualifiedContainerKey(runtimeParent, name)
		spec := ContainerSpecFor(wf, composeFiles, name)
		spec.Name = handleKey
		h, err := createOrRestoreRuntimeHandle(ctx, ld, wf.Containers[name], spec, handleKey, runstate)
		if err != nil {
			destroyErr := destroyRuntimeHandles(ld.Backend, handles)
			if destroyErr != nil {
				err = errors.Join(err, destroyErr)
			}
			return handles, fmt.Errorf("create child container %q: %w", name, err)
		}
		handles[handleKey] = h
	}
	return handles, nil
}

func createOrRestoreRuntimeHandle(ctx context.Context, ld *LocalDispatcher, ctr ir.Container, spec container.ContainerSpec, handleKey string, runstate *RunState) (container.Handle, error) {
	if ctr.Snapshot == "workspace" && runstate != nil {
		if ref := runstate.SnapshotRefs[handleKey]; ref != "" {
			return ld.Backend.Restore(ctx, container.SnapshotRef(ref), handleKey)
		}
	}
	return ld.Backend.Create(ctx, spec)
}

func destroyRuntimeHandles(backend container.Backend, handles map[string]container.Handle) error {
	var out error
	for key, h := range handles {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), container.TeardownGrace)
		err := backend.Destroy(cleanupCtx, h)
		cancel()
		if err != nil {
			out = errors.Join(out, fmt.Errorf("destroy child container %q: %w", key, err))
		}
	}
	return out
}

func cloneDispatcherForRuntime(parent *LocalDispatcher, runtimeParent string, composeFiles map[string][]byte, childHandles map[string]container.Handle) *LocalDispatcher {
	handles := make(map[string]container.Handle, len(parent.Handles)+len(childHandles))
	for k, v := range parent.Handles {
		handles[k] = v
	}
	for k, v := range childHandles {
		handles[k] = v
	}
	return &LocalDispatcher{
		Backend:          parent.Backend,
		Handles:          handles,
		RuntimeParent:    runtimeParent,
		ComposeFiles:     composeFiles,
		Resolver:         parent.Resolver,
		AgentEventTap:    parent.AgentEventTap,
		RenderAgentEvent: parent.RenderAgentEvent,
		StepCostLine:     parent.StepCostLine,
		RunState:         parent.RunState,
		Blobs:            parent.Blobs,
	}
}
