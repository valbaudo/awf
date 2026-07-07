package cli

import (
	"fmt"
	"io"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// checkIdleLiveness is the advisory idle-vs-liveness preflight. It walks every
// agent step; for each step that sets an author `timeout.idle` whose resolved
// adapter is BLIND (Caps.SurfacesLiveness == agent.LivenessNone), it prints a
// warning to stderr. A blind adapter emits no live progress signal the idle
// watchdog can trust as proof the turn is still working, so the idle timer
// never resets — it degenerates into a wall-clock deadline that can kill a
// step making silent progress. Coarse/Fine adapters surface enough liveness
// for idle to mean what the author intends, so they are not warned; a step
// with no idle: is skipped (the author opted into nothing).
//
// Advisory-only: returns nil always — like the credential-presence preflight,
// it never fails a run. It carries the AWF3016 code for grep/CI consumers.
func checkIdleLiveness(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	return checkIdleLivenessForLoadedDefinition(ld, resolver, stderr)
}

func checkIdleLivenessForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil || module.Workflow == nil {
			return nil
		}
		walkIdleLivenessNodes(module.Workflow, module.ID, module.Workflow.Graph, resolver, stderr)
		return nil
	})
}

func walkIdleLivenessNodes(wf *ir.Workflow, moduleID string, nodes ir.NodeList, resolver agent.Resolver, stderr io.Writer) {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.Timeout == nil || v.Timeout.Idle == nil {
				continue // no author idle: — nothing to advise
			}
			ref := engine.AgentRuntimeRef(wf, moduleID, v.Uses)
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				continue // unresolved adapter — resolveRuntimes will hard-error; skip silently
			}
			if adapter.Capabilities().SurfacesLiveness != agent.LivenessNone {
				continue // Coarse/Fine surfaces liveness — idle means what the author intends
			}
			_, _ = fmt.Fprintf(stderr, "Warning: AWF3016 agent step %q: adapter %q surfaces no liveness signal, so its timeout.idle behaves as a wall-clock deadline (a silent-but-working turn can be killed); drop idle or set a wall timeout instead\n", v.ID, ref)
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip, *ir.React:
			// no agent step; no idle-liveness check needed
		case *ir.If:
			walkIdleLivenessNodes(wf, moduleID, v.Then, resolver, stderr)
			walkIdleLivenessNodes(wf, moduleID, v.Else, resolver, stderr)
		case *ir.Loop:
			walkIdleLivenessNodes(wf, moduleID, v.Body, resolver, stderr)
		case *ir.Try:
			walkIdleLivenessNodes(wf, moduleID, v.Do, resolver, stderr)
			walkIdleLivenessNodes(wf, moduleID, v.Catch, resolver, stderr)
			walkIdleLivenessNodes(wf, moduleID, v.Finally, resolver, stderr)
		case *ir.Parallel:
			walkIdleLivenessNodes(wf, moduleID, v.Children, resolver, stderr)
		case *ir.Gate:
			walkIdleLivenessNodes(wf, moduleID, v.Generate, resolver, stderr)
			walkIdleLivenessNodes(wf, moduleID, v.Evaluate, resolver, stderr)
		case *ir.Map:
			walkIdleLivenessNodes(wf, moduleID, v.Body, resolver, stderr)
		case *ir.Compose:
			walkIdleLivenessNodes(wf, moduleID, v.Body, resolver, stderr)
		default:
			// Unreachable from outside ir/ — ir.Node is a closed sum type.
			// Keep this switch in sync with credential_guard.go's traversal.
			panic(fmt.Sprintf("walkIdleLivenessNodes: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
}
