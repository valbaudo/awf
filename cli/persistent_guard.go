package cli

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

type ErrPersistentSessionGateEvaluate struct {
	StepID string
	Ref    string
}

func (e *ErrPersistentSessionGateEvaluate) Error() string {
	return fmt.Sprintf("cli: step %q is in gate.evaluate but its agent runtime %q declares persistent session support (Caps.PersistentSession is true)", e.StepID, e.Ref)
}

func checkPersistentSessionGateEvaluate(wf *ir.Workflow, resolver agent.Resolver) error {
	return checkPersistentSessionGateEvaluateForLoadedDefinition(&ir.LoadedDefinition{Workflow: wf}, resolver)
}

func checkPersistentSessionGateEvaluateForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil {
			return nil
		}
		if err := checkPersistentSessionWorkflow(ld, module.Workflow, module.ID, resolver); err != nil {
			if module.ID != "" {
				return fmt.Errorf("module %s: %w", module.ID, err)
			}
			return err
		}
		return nil
	})
}

func checkPersistentSessionWorkflow(ld *ir.LoadedDefinition, wf *ir.Workflow, moduleID string, resolver agent.Resolver) error {
	if wf == nil {
		return nil
	}
	return checkPersistentSessionNodes(ld, wf, moduleID, wf.Graph, resolver, false)
}

func checkPersistentSessionNodes(ld *ir.LoadedDefinition, wf *ir.Workflow, moduleID string, nodes ir.NodeList, resolver agent.Resolver, inGateEvaluate bool) error {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if !inGateEvaluate {
				continue
			}
			ref := engine.AgentRuntimeRef(wf, moduleID, v.Uses)
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				return &agent.ErrAdapterNotFound{Ref: ref}
			}
			if adapter.Capabilities().PersistentSession {
				return &ErrPersistentSessionGateEvaluate{StepID: v.ID, Ref: ref}
			}
		case *ir.CallStep:
			if inGateEvaluate && ld != nil {
				child, ok := persistentGuardCallTargetModule(ld, moduleID, v.Call)
				if ok {
					if err := checkPersistentSessionNodes(ld, child.Workflow, child.ID, child.Workflow.Graph, resolver, true); err != nil {
						return err
					}
				}
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.Skip, *ir.React:
			// react runs on awf/llm (Containerless+Threaded, never PersistentSession)
			// and has no NodeList body — nothing for the gate.evaluate persistent-
			// session guard to check.
		case *ir.If:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Then, resolver, inGateEvaluate); err != nil {
				return err
			}
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Else, resolver, inGateEvaluate); err != nil {
				return err
			}
		case *ir.Loop:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Body, resolver, inGateEvaluate); err != nil {
				return err
			}
		case *ir.Try:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Do, resolver, inGateEvaluate); err != nil {
				return err
			}
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Catch, resolver, inGateEvaluate); err != nil {
				return err
			}
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Finally, resolver, inGateEvaluate); err != nil {
				return err
			}
		case *ir.Parallel:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Children, resolver, inGateEvaluate); err != nil {
				return err
			}
		case *ir.Gate:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Generate, resolver, inGateEvaluate); err != nil {
				return err
			}
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Evaluate, resolver, true); err != nil {
				return err
			}
		case *ir.Map:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Body, resolver, inGateEvaluate); err != nil {
				return err
			}
		case *ir.Compose:
			if err := checkPersistentSessionNodes(ld, wf, moduleID, v.Body, resolver, inGateEvaluate); err != nil {
				return err
			}
		default:
			panic(fmt.Sprintf("checkPersistentSessionGateEvaluate: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
	return nil
}

func persistentGuardCallTargetModule(ld *ir.LoadedDefinition, parentID, importID string) (*ir.LoadedModule, bool) {
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
