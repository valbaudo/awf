package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

const callTeardownGrace = 30 * time.Second

func createRuntimeHandles(ctx context.Context, ld *LocalDispatcher, wf *ir.Workflow, composeFiles map[string][]byte, runtimeParent string) (map[string]container.Handle, error) {
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
		spec := ContainerSpecFor(wf, composeFiles, name)
		spec.Name = QualifiedContainerKey(runtimeParent, name)
		h, err := ld.Backend.Create(ctx, spec)
		if err != nil {
			destroyErr := destroyRuntimeHandles(ld.Backend, handles)
			if destroyErr != nil {
				err = errors.Join(err, destroyErr)
			}
			return handles, fmt.Errorf("create child container %q: %w", name, err)
		}
		handles[QualifiedContainerKey(runtimeParent, name)] = h
	}
	return handles, nil
}

func destroyRuntimeHandles(backend container.Backend, handles map[string]container.Handle) error {
	var out error
	for key, h := range handles {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), callTeardownGrace)
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
	}
}
