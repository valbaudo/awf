package cli

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
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
	if wf == nil {
		return nil
	}
	return checkThreadedNodes(wf.Graph, resolver)
}

// checkThreadedNodes is the recursive worker. It descends into every
// structural-node type that can contain steps. Unlike walkAgentRefsNodes
// (cli/runtimes.go), it DOES descend into map.body: a continues: step inside a
// map body still resolves an adapter and still needs Threaded. This switch must
// stay in sync with engine/scope.go's node walker; when ir/ adds a new node
// type, update both (the default arm is unreachable from outside ir/ — ir.Node
// is a closed sum type with an unexported isNode() marker).
func checkThreadedNodes(nodes ir.NodeList, resolver agent.Resolver) error {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.Continues == "" {
				continue
			}
			adapter, ok := resolver.Lookup(v.Uses)
			if !ok {
				return &agent.ErrAdapterNotFound{Ref: v.Uses}
			}
			if !adapter.Capabilities().Threaded {
				return &ErrThreadedRequired{StepID: v.ID, Ref: v.Uses}
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.Skip:
			// no nested steps; cannot declare continues:
		case *ir.If:
			if err := checkThreadedNodes(v.Then, resolver); err != nil {
				return err
			}
			if err := checkThreadedNodes(v.Else, resolver); err != nil {
				return err
			}
		case *ir.Loop:
			if err := checkThreadedNodes(v.Body, resolver); err != nil {
				return err
			}
		case *ir.Try:
			if err := checkThreadedNodes(v.Do, resolver); err != nil {
				return err
			}
			if err := checkThreadedNodes(v.Catch, resolver); err != nil {
				return err
			}
			if err := checkThreadedNodes(v.Finally, resolver); err != nil {
				return err
			}
		case *ir.Parallel:
			if err := checkThreadedNodes(v.Children, resolver); err != nil {
				return err
			}
		case *ir.Gate:
			if err := checkThreadedNodes(v.Generate, resolver); err != nil {
				return err
			}
			if err := checkThreadedNodes(v.Evaluate, resolver); err != nil {
				return err
			}
		case *ir.Map:
			// Descend into the map body — a continues: step inside a fan-out
			// branch (E.2) still needs a Threaded adapter.
			if err := checkThreadedNodes(v.Body, resolver); err != nil {
				return err
			}
		default:
			// Unreachable from outside ir/ (ir.Node is a closed sum type with an
			// unexported isNode() marker). Defensive: mirror walkAgentRefsNodes /
			// engine/scope.go when a new node type lands.
			panic(fmt.Sprintf("checkThreadedAdapters: unhandled ir.Node type %T (extend the switch; mirror cli/runtimes.go walkAgentRefsNodes and engine/scope.go)", n))
		}
	}
	return nil
}
