package cli

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// checkThreadedAdapters fails fast (run start AND resume) if any agent step
// declares `continues:` while its resolved adapter reports Caps.Threaded ==
// false. Part D of the continues: design: the engine never branches on Caps at
// runtime, so an un-threaded adapter would silently drop the assembled
// AgentInvocation.Thread; this guard turns that into a hard error instead.
//
// Mirrors the Containerless guard (resolveRuntimes -> ErrContainerRequired) but
// is keyed per-STEP, not per-(uses,container) pair: resolveRuntimes dedupes
// pairs and has discarded the per-step continues: bit by the time it runs, so
// the threaded check needs its own walk. Returns:
//   - *ErrThreadedRequired on the first non-threaded continues: step (document order),
//   - *agent.ErrAdapterNotFound if a continues: step's uses: resolves to no adapter
//     (a Lookup miss must not be read as "not threaded"),
//   - nil if no step declares continues:, or every such adapter is Threaded.
func checkThreadedAdapters(wf *ir.Workflow, resolver agent.Resolver) error {
	return checkThreadedAdaptersForLoadedDefinition(&ir.LoadedDefinition{Workflow: wf}, resolver)
}

func checkThreadedAdaptersForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil {
			return nil
		}
		if err := checkThreadedWorkflow(module.Workflow, module.ID, resolver); err != nil {
			if module.ID != "" {
				return fmt.Errorf("module %s: %w", module.ID, err)
			}
			return err
		}
		return nil
	})
}

func checkThreadedWorkflow(wf *ir.Workflow, moduleID string, resolver agent.Resolver) error {
	if wf == nil {
		return nil
	}
	stepsByID := collectAgentSteps(wf.Graph, nil)
	return checkThreadedNodes(wf, moduleID, wf.Graph, resolver, stepsByID)
}

// checkThreadedNodes is the recursive worker. It descends into every
// structural-node type that can contain steps: a continues: step inside a map
// body still resolves an adapter and still needs Threaded. This switch must
// stay in sync with the runtime-ref traversal; when ir/ adds a new node type,
// update both (the default arm is unreachable from outside ir/ — ir.Node is a
// closed sum type with an unexported isNode() marker).
func checkThreadedNodes(wf *ir.Workflow, moduleID string, nodes ir.NodeList, resolver agent.Resolver, stepsByID map[string]*ir.AgentStep) error {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.Continues == "" {
				continue
			}
			ref := engine.AgentRuntimeRef(wf, moduleID, v.Uses)
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				return &agent.ErrAdapterNotFound{Ref: ref}
			}
			if !adapter.Capabilities().Threaded {
				return &ErrThreadedRequired{StepID: v.ID, Ref: ref}
			}
			if target := stepsByID[v.Continues]; target != nil {
				targetRef := engine.AgentRuntimeRef(wf, moduleID, target.Uses)
				targetAdapter, ok := resolver.Lookup(targetRef)
				if !ok {
					return &agent.ErrAdapterNotFound{Ref: targetRef}
				}
				if targetAdapter.Capabilities().PersistentSession {
					return &ErrPersistentSessionContinuesTarget{StepID: v.ID, TargetID: v.Continues, Ref: targetRef}
				}
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip, *ir.React:
			// no nested steps; cannot declare continues:. react has no continues:
			// field and no NodeList body — its awf/llm adapter is Threaded by
			// construction (gated by the run-start Containerless+Threaded assertion,
			// Phase 4), so there is nothing for THIS guard (continues: → Threaded) to
			// check on a react node.
		case *ir.If:
			if err := checkThreadedNodes(wf, moduleID, v.Then, resolver, stepsByID); err != nil {
				return err
			}
			if err := checkThreadedNodes(wf, moduleID, v.Else, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Loop:
			if err := checkThreadedNodes(wf, moduleID, v.Body, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Try:
			if err := checkThreadedNodes(wf, moduleID, v.Do, resolver, stepsByID); err != nil {
				return err
			}
			if err := checkThreadedNodes(wf, moduleID, v.Catch, resolver, stepsByID); err != nil {
				return err
			}
			if err := checkThreadedNodes(wf, moduleID, v.Finally, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Parallel:
			if err := checkThreadedNodes(wf, moduleID, v.Children, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Gate:
			if err := checkThreadedNodes(wf, moduleID, v.Generate, resolver, stepsByID); err != nil {
				return err
			}
			if err := checkThreadedNodes(wf, moduleID, v.Evaluate, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Map:
			// Descend into the map body — a continues: step inside a fan-out
			// branch (E.2) still needs a Threaded adapter.
			if err := checkThreadedNodes(wf, moduleID, v.Body, resolver, stepsByID); err != nil {
				return err
			}
		case *ir.Compose:
			if err := checkThreadedNodes(wf, moduleID, v.Body, resolver, stepsByID); err != nil {
				return err
			}
		default:
			// Unreachable from outside ir/ (ir.Node is a closed sum type with an
			// unexported isNode() marker). Defensive: keep this switch in sync with
			// the runtime-ref traversal when a new ir.Node type lands.
			panic(fmt.Sprintf("checkThreadedAdapters: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
	return nil
}

func collectAgentSteps(nodes ir.NodeList, out map[string]*ir.AgentStep) map[string]*ir.AgentStep {
	if out == nil {
		out = map[string]*ir.AgentStep{}
	}
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.ID != "" {
				out[v.ID] = v
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip, *ir.React:
			// react is not an AgentStep — it cannot be a continues: target, so it is
			// not collected into the continues-resolution index.
		case *ir.If:
			collectAgentSteps(v.Then, out)
			collectAgentSteps(v.Else, out)
		case *ir.Loop:
			collectAgentSteps(v.Body, out)
		case *ir.Try:
			collectAgentSteps(v.Do, out)
			collectAgentSteps(v.Catch, out)
			collectAgentSteps(v.Finally, out)
		case *ir.Parallel:
			collectAgentSteps(v.Children, out)
		case *ir.Gate:
			collectAgentSteps(v.Generate, out)
			collectAgentSteps(v.Evaluate, out)
		case *ir.Map:
			collectAgentSteps(v.Body, out)
		case *ir.Compose:
			collectAgentSteps(v.Body, out)
		default:
			panic(fmt.Sprintf("collectAgentSteps: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
	return out
}
