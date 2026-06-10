package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
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

// resolverOrEmpty returns r.Resolver if set, else a freshly-allocated empty
// *agent.Registry. The empty fallback exists so workflows without any
// `uses:` step (every Phase 2-4 fixture) work unchanged — they trigger zero
// Lookup calls. Workflows WITH a `uses:` step, run against an empty Resolver,
// fail at run-start with *ErrAdapterNotFound (resolveRuntimes wraps this).
// Production wiring lives in slice 5.3 (cli/agent_registry.go), which
// constructs a populated *agent.Registry and assigns it to r.Resolver
// before dispatch. Called by both cli/run.go (run-start resolution) and
// cli/resume.go (resume-side re-resolution for the drift check).
func (r *Runner) resolverOrEmpty() agent.Resolver {
	if r.Resolver != nil {
		return r.Resolver
	}
	return &agent.Registry{}
}

// walkAgentRefs returns the distinct (uses, container) pairs declared by
// any AgentStep in the workflow's Graph (recursively, through If/Loop/
// Try/Parallel/Gate/Map structural nodes). Sorted by (Uses, Container) for
// deterministic golden tests. A workflow with no `uses:` steps returns
// nil (NOT empty slice — `omitempty` on the consumer side then writes
// "runtimes" absent from the run.started JSON).
func walkAgentRefs(wf *ir.Workflow) []agentRef {
	refs := engine.WalkRuntimeRefs("", "", wf)
	if len(refs) == 0 {
		return nil
	}
	out := make([]agentRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, agentRef{Uses: ref.Uses, Container: ref.Container})
	}
	return out
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
	engineRefs := make([]engine.RuntimeRef, 0, len(refs))
	for _, ref := range refs {
		engineRefs = append(engineRefs, engine.RuntimeRef{Uses: ref.Uses, Container: ref.Container})
	}
	out, err := engine.ResolveRuntimes(ctx, engineRefs, resolver, handles)
	var required *engine.ErrContainerRequired
	if errors.As(err, &required) {
		return nil, &ErrContainerRequired{Ref: required.Ref}
	}
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return out, nil
}

// checkRuntimesDrift compares the recorded Runtimes (from run.started in the
// log) against the freshly-resolved current Runtimes (this resume's
// resolveRuntimes call). Returns *ErrRuntimeDrift on the first mismatch.
//
// Compares element-by-element after asserting equal length and matching
// (Ref, Container) pairs in the same order. resolveRuntimes guarantees
// sorted output (via walkAgentRefs's slices.SortFunc), so order comparison is
// safe.
//
// Drift forms detected:
//   - Different Version for the same (Ref, Container)
//   - Different (Ref, Container) pair present
//   - Different length (workflow added or removed a `uses:` ref)
func checkRuntimesDrift(recorded, current []engine.ResolvedRuntime) error {
	err := engine.CheckRuntimesDrift(recorded, current)
	var drift *engine.ErrRuntimeDrift
	if errors.As(err, &drift) {
		return &ErrRuntimeDrift{
			Ref:       drift.Ref,
			Container: drift.Container,
			Recorded:  drift.Recorded,
			Current:   drift.Current,
		}
	}
	if err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	return nil
}

// readRuntimesFromLog extracts the Runtimes field from the run.started
// event in a folded log. Returns nil + nil on:
//   - empty events (zero-event log — pre-Step-1 corruption)
//   - first event not run.started (corruption — caller can check separately)
//
// Returns (nil, error) on unmarshal failure (log corruption).
// Returns the runtimes slice on success (possibly empty if no `uses:` refs
// in the original run — Phase 2-4 logs decode to nil). Mirrors the
// readBackendKindFromLog(cli/backend.go) pattern from slice 4.5.
func readRuntimesFromLog(events []state.Event) ([]engine.ResolvedRuntime, error) {
	if len(events) == 0 || events[0].Type != engine.EventRunStarted {
		return nil, nil
	}
	var rs engine.RunStartedData
	if err := json.Unmarshal(events[0].Data, &rs); err != nil {
		return nil, fmt.Errorf("cli: unmarshal run.started for runtimes: %w", err)
	}
	return rs.Runtimes, nil
}
