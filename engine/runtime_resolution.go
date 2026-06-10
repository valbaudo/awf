package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

type RuntimeRef struct {
	ModuleID      string
	NodePath      string
	RuntimeParent string
	Uses          string
	Container     string
}

type ErrContainerRequired struct {
	Ref string
}

func (e *ErrContainerRequired) Error() string {
	return fmt.Sprintf("engine: agent runtime %q requires a container, but the step declares none (only Containerless adapters may omit `container:`)", e.Ref)
}

type ErrRuntimeDrift struct {
	Ref       string
	Container string
	Recorded  string
	Current   string
}

func (e *ErrRuntimeDrift) Error() string {
	return fmt.Sprintf("engine: agent runtime drift for %q in container %q: recorded %q, now %q", e.Ref, e.Container, e.Recorded, e.Current)
}

func AgentRuntimeRef(wf *ir.Workflow, moduleID, uses string) string {
	if wf == nil {
		return uses
	}
	if _, ok := wf.RoleByName(uses); !ok {
		return uses
	}
	if moduleID == RootModuleID {
		return uses
	}
	return fmt.Sprintf("awf:role:%d:%s:%s", len(moduleID), moduleID, uses)
}

func WalkRuntimeRefs(moduleID, runtimeParent string, wf *ir.Workflow) []RuntimeRef {
	if wf == nil {
		return nil
	}
	seen := map[runtimeRefKey]RuntimeRef{}
	walkRuntimeRefsNodes(wf, moduleID, runtimeParent, wf.Graph, "", nil, seen)
	if len(seen) == 0 {
		return nil
	}
	out := make([]RuntimeRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b RuntimeRef) int {
		if c := cmp.Compare(a.Uses, b.Uses); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Container, b.Container); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ModuleID, b.ModuleID); c != 0 {
			return c
		}
		return cmp.Compare(a.NodePath, b.NodePath)
	})
	return out
}

type runtimeRefKey struct {
	moduleID      string
	runtimeParent string
	uses          string
	container     string
}

func walkRuntimeRefsNodes(wf *ir.Workflow, moduleID, runtimeParent string, nodes ir.NodeList, parent string, runtimeCreatedContainers map[string]bool, seen map[runtimeRefKey]RuntimeRef) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			bare, _ := SplitContainerRef(v.Container)
			if bare != "" && runtimeCreatedContainers[bare] {
				continue
			}
			path := ir.PathFor(parent, "", v.ID, i)
			uses := AgentRuntimeRef(wf, moduleID, v.Uses)
			ref := RuntimeRef{ModuleID: moduleID, NodePath: path, RuntimeParent: runtimeParent, Uses: uses, Container: v.Container}
			key := runtimeRefKey{moduleID: moduleID, runtimeParent: runtimeParent, uses: uses, container: v.Container}
			if _, ok := seen[key]; !ok {
				seen[key] = ref
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip:
		case *ir.If:
			base := ir.PathFor(parent, "if", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Then, base+".then", runtimeCreatedContainers, seen)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Else, base+".else", runtimeCreatedContainers, seen)
		case *ir.Loop:
			base := ir.PathFor(parent, "loop", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Body, base+".body", runtimeCreatedContainers, seen)
		case *ir.Try:
			base := ir.PathFor(parent, "try", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Do, base+".do", runtimeCreatedContainers, seen)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Catch, base+".catch", runtimeCreatedContainers, seen)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Finally, base+".finally", runtimeCreatedContainers, seen)
		case *ir.Parallel:
			base := ir.PathFor(parent, "parallel", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Children, base, runtimeCreatedContainers, seen)
		case *ir.Gate:
			base := ir.PathFor(parent, "gate", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Generate, base+".generate", runtimeCreatedContainers, seen)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Evaluate, base+".evaluate", runtimeCreatedContainers, seen)
		case *ir.Map:
			base := ir.PathFor(parent, "map", "", i)
			nextRuntimeCreated := runtimeCreatedContainers
			if v.Image != "" {
				nextRuntimeCreated = withRuntimeCreatedContainer(runtimeCreatedContainers, v.Container)
			}
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Body, base+".body", nextRuntimeCreated, seen)
		case *ir.Compose:
			base := ir.PathFor(parent, "compose", "", i)
			walkRuntimeRefsNodes(wf, moduleID, runtimeParent, v.Body, base+".body", withRuntimeCreatedContainer(runtimeCreatedContainers, v.As), seen)
		default:
			panic(fmt.Sprintf("engine.WalkRuntimeRefs: unhandled ir.Node type %T", n))
		}
	}
}

func withRuntimeCreatedContainer(existing map[string]bool, name string) map[string]bool {
	if name == "" {
		return existing
	}
	next := make(map[string]bool, len(existing)+1)
	for k, v := range existing {
		next[k] = v
	}
	next[name] = true
	return next
}

func ResolveRuntimes(ctx context.Context, refs []RuntimeRef, resolver agent.Resolver, handles map[string]container.Handle) ([]ResolvedRuntime, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if resolver == nil {
		resolver = &agent.Registry{}
	}
	out := make([]ResolvedRuntime, 0, len(refs))
	for _, ref := range refs {
		adapter, ok := resolver.Lookup(ref.Uses)
		if !ok {
			return nil, &agent.ErrAdapterNotFound{Ref: ref.Uses}
		}
		if ref.Container == "" {
			if !adapter.Capabilities().Containerless {
				return nil, &ErrContainerRequired{Ref: ref.Uses}
			}
			ver, err := adapter.Version(ctx, container.Handle{})
			if err != nil {
				return nil, fmt.Errorf("engine: adapter %q version resolution (module=%q path=%q containerless): %w", ref.Uses, ref.ModuleID, ref.NodePath, err)
			}
			out = append(out, ResolvedRuntime{Ref: ref.Uses, Version: ver})
			continue
		}
		bare, svc := SplitContainerRef(ref.Container)
		key := QualifiedContainerKey(ref.RuntimeParent, bare)
		handle, ok := handles[key]
		if !ok {
			return nil, fmt.Errorf("engine: no handle for container %q (key %q, module=%q path=%q)", ref.Container, key, ref.ModuleID, ref.NodePath)
		}
		if svc != "" {
			handle.Service = svc
		}
		ver, err := adapter.Version(ctx, handle)
		if err != nil {
			return nil, fmt.Errorf("engine: adapter %q version resolution in container %q (module=%q path=%q): %w", ref.Uses, key, ref.ModuleID, ref.NodePath, err)
		}
		out = append(out, ResolvedRuntime{
			Ref:       ref.Uses,
			Version:   ver,
			Container: QualifiedContainerKey(ref.RuntimeParent, ref.Container),
		})
	}
	return out, nil
}

func CheckRuntimesDrift(recorded, current []ResolvedRuntime) error {
	if len(recorded) != len(current) {
		return fmt.Errorf("engine: agent runtime set drift: recorded %d runtime(s), now %d", len(recorded), len(current))
	}
	for i := range recorded {
		r := recorded[i]
		c := current[i]
		if r.Ref != c.Ref || r.Container != c.Container {
			return fmt.Errorf("engine: agent runtime set drift: recorded (ref=%q, container=%q), now (ref=%q, container=%q)", r.Ref, r.Container, c.Ref, c.Container)
		}
		if r.Version != c.Version {
			return &ErrRuntimeDrift{Ref: r.Ref, Container: r.Container, Recorded: r.Version, Current: c.Version}
		}
	}
	return nil
}
