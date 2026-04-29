package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// agentRef is one distinct `(uses, container)` pair extracted from a
// workflow's Graph. The CLI uses these to (a) call Adapter.Version per pair
// at run start and persist into RunStartedData.Runtimes, (b) re-resolve and
// compare on resume.
//
// Private — only the CLI uses this type. The engine works with the
// fully-resolved engine.ResolvedRuntime slice; the CLI translates between
// agentRef + Version outputs and engine.ResolvedRuntime in resolveRuntimes.
type agentRef struct {
	Uses      string
	Container string
}

// walkAgentRefs returns the distinct (uses, container) pairs declared by
// any AgentStep in the workflow's Graph (recursively, through If/Loop/
// Try/Parallel/Gate/Map structural nodes). Sorted by (Uses, Container) for
// deterministic golden tests. A workflow with no `uses:` steps returns
// nil (NOT empty slice — `omitempty` on the consumer side then writes
// "runtimes" absent from the run.started JSON).
func walkAgentRefs(wf *ir.Workflow) []agentRef {
	if wf == nil {
		return nil
	}
	seen := map[agentRef]bool{}
	walkAgentRefsNodes(wf.Graph, seen)
	if len(seen) == 0 {
		return nil
	}
	out := make([]agentRef, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	// Go 1.21+ idiom: slices.SortFunc with cmp.Compare composes naturally —
	// the cmp.Compare result chain (return early on the first non-zero) is
	// clearer than the boolean less-than chain it replaces. Stable ordering
	// matters because the resolved slice is byte-compared on resume.
	slices.SortFunc(out, func(a, b agentRef) int {
		if c := cmp.Compare(a.Uses, b.Uses); c != 0 {
			return c
		}
		return cmp.Compare(a.Container, b.Container)
	})
	return out
}

// walkAgentRefsNodes is the recursive worker. It descends into every
// structural-node type that can contain steps; step-typed leaves contribute
// to seen.
//
// MAP NODES — AgentSteps inside `map.body` are intentionally SKIPPED at
// run-start version pinning. Rationale (Phase 5 slice 5.1 design decision):
// Map's per-item containers are created at dispatch time (spec §5.7 — "each
// element runs body in its own container instance"). They don't exist at
// run-start; we have no handle to pass to Adapter.Version(). The deeper
// reason this is SAFE rather than a coverage gap: every per-item container
// is reconstructed from the IR-declared container's `image:` digest, which
// Phase 1.4's validator ALREADY pins (spec §3 — every image must be content-
// addressed). The `claude` binary baked into a digest-pinned image cannot
// drift across resumes — the image digest IS the version pin. So
// Adapter.Version() would add no protection beyond what the digest pin
// already provides; skipping is correct, not lazy.
//
// UNKNOWN NODE — PANIC, but the default arm is UNREACHABLE from outside
// the ir package. ir.Node is `interface{ isNode() }` (see ir/node.go:17) —
// a CLOSED sum type with an unexported marker method. No type outside
// the ir package can satisfy ir.Node, so the default arm cannot be
// unit-tested from cli/. The panic exists as forward-compatibility
// documentation: when ir/ adds a new node type (extending the closed
// set), this switch MUST be updated. If the case is missed, real IR
// fixtures exercising the new type panic loudly at run-start — caught by
// any integration test or fixture run that exercises the new type. This
// switch MUST stay in sync with engine/scope.go's node walker (which
// traverses the same node types for Phase 3.2+ scope resolution); when
// adding a new ir.Node type, update both walkers.
func walkAgentRefsNodes(nodes ir.NodeList, seen map[agentRef]bool) {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			seen[agentRef{Uses: v.Uses, Container: v.Container}] = true
		case *ir.CodeStep, *ir.SignalStep, *ir.Skip:
			// no nested steps
		case *ir.If:
			walkAgentRefsNodes(v.Then, seen)
			walkAgentRefsNodes(v.Else, seen)
		case *ir.Loop:
			walkAgentRefsNodes(v.Body, seen)
		case *ir.Try:
			walkAgentRefsNodes(v.Do, seen)
			walkAgentRefsNodes(v.Catch, seen)
			walkAgentRefsNodes(v.Finally, seen)
		case *ir.Parallel:
			// ir.Parallel has a single `Children NodeList` (the standard's
			// `{"parallel":[<node>,...]}` shape — flat list of branch heads).
			walkAgentRefsNodes(v.Children, seen)
		case *ir.Gate:
			walkAgentRefsNodes(v.Generate, seen)
			walkAgentRefsNodes(v.Evaluate, seen)
		case *ir.Map:
			// Map body intentionally NOT traversed — per-item containers are
			// dispatch-time, version-pinned via image digest (Phase 1.4).
			// See doc-comment above for the safety argument.
			_ = v
		default:
			// Unreachable from outside ir/ (ir.Node is closed sum type with
			// unexported isNode() marker). Defensive documentation only.
			// See doc-comment on walkAgentRefsNodes above.
			panic(fmt.Sprintf("walkAgentRefs: unhandled ir.Node type %T (extend the switch; mirror engine/scope.go)", n))
		}
	}
}

// resolveRuntimes calls Adapter.Version once per (ref, container) pair and
// returns the slice ready to write into RunStartedData.Runtimes.
//
// Called twice per run lifecycle: once at run start (cli/run.go writes the
// result into the persistent run.started event), once at resume (cli/resume.go
// compares the result to the persisted Runtimes and hard-errors on drift —
// spec §8 pinning).
//
// Returns *agent.ErrAdapterNotFound (Lookup miss) or a wrapped error if
// Adapter.Version fails for some reason (network, missing binary, etc.).
// Missing handle is a wiring bug — the caller must Create handles for every
// container before calling this.
func resolveRuntimes(ctx context.Context, refs []agentRef, resolver agent.Resolver, handles map[string]container.Handle) ([]engine.ResolvedRuntime, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]engine.ResolvedRuntime, 0, len(refs))
	for _, ref := range refs {
		adapter, ok := resolver.Lookup(ref.Uses)
		if !ok {
			return nil, &agent.ErrAdapterNotFound{Ref: ref.Uses}
		}
		handle, ok := handles[ref.Container]
		if !ok {
			return nil, fmt.Errorf("cli: no handle for container %q (resolveRuntimes wiring bug — Create the container before calling)", ref.Container)
		}
		ver, err := adapter.Version(ctx, handle)
		if err != nil {
			return nil, fmt.Errorf("cli: adapter %q version resolution in container %q: %w", ref.Uses, ref.Container, err)
		}
		out = append(out, engine.ResolvedRuntime{
			Ref:       ref.Uses,
			Version:   ver,
			Container: ref.Container,
		})
	}
	return out, nil
}
