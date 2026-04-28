package agent

import (
	"context"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// Adapter is the seam the engine's dispatcher uses to launch external agent
// runtimes (Claude Code, Goose, Codex, Droid — Phase 5 ships only Claude
// Code, slice 5.3). Each concrete Adapter wraps one harness CLI; the
// interface stays uniform across them so a workflow's `uses:` refs can be
// satisfied by any registered adapter matching the Ref() string.
//
// Lifecycle in slice 5.2 dispatcher (runAgentStep):
//
//  1. resolver.Lookup(node.Uses) → Adapter (else *ErrAdapterNotFound)
//  2. adapter.ValidateConfig(with) once per node, at run-start (CLI walk)
//     and at dispatch time (defensive) — strict schema validation.
//  3. adapter.Launch(ctx, handle, inv) per attempt (one per gate attempt;
//     one per retry). The returned <-chan AgentEvent stays open while the
//     harness produces events; it MUST be drained by the caller (the
//     dispatcher) and closed by the adapter before AgentResult is
//     returned synchronously.
//
// Phase 5 design decision 7: Adapter.Launch MUST NOT reuse sessions
// across calls. Each Launch is a fresh context — for Claude Code this
// means no --continue / --resume / --session-id flags; ValidateConfig
// rejects any with-key that would enable session reuse. This is what
// makes the gate's "independence" property (spec §5.5) structural rather
// than convention.
//
// Phase 5 design decision 16: Capabilities() reports static claims about
// the adapter's typed-output pipeline. The engine doesn't branch on it;
// the conformance suite routes adapters to bucket 14 (NativeSchema=true)
// or bucket 15 (NativeSchema=false).
type Adapter interface {
	// Ref returns the agent-runtime identifier. Must match the workflow's
	// AgentStep.Uses literal exactly (e.g. "anthropic/claude-code"). Stable
	// over the adapter's lifetime; Registry uses this as the key.
	Ref() string

	// Capabilities reports static claims about this adapter's behavior.
	// Must be callable on a freshly-constructed adapter (no per-invocation
	// state needed). Tested by Bucket 14/15 routing in slice 5.4 + 5.2.
	Capabilities() Caps

	// Version returns the concrete version string this adapter resolves to
	// for the given container handle (the binary's PATH is per-container).
	// Called once per (ref, container) pair at run start; the result is
	// persisted in run.started.Runtimes. Resume re-invokes Version and
	// hard-errors on drift (spec §8 pinning). Adapters that don't have a
	// natural binary version (the fake) return a constructor-supplied
	// constant; adapters wrapping a CLI return the CLI's --version output
	// or equivalent.
	//
	// The handle is passed so an adapter wrapping a containerized binary
	// can `Backend.Exec(handle, "claude --version")` to discover the
	// version in the binary's actual deployment environment. Slice 5.1's
	// fake adapter ignores handle.
	Version(ctx context.Context, handle container.Handle) (string, error)

	// ValidateConfig is called once per node at run start AND defensively
	// at dispatch (slice 5.2). Returns *ErrInvalidConfig with a path-aware
	// message on rejection. Strict by default: unknown keys are errors
	// (slice 5.3 enforces this for the Claude Code adapter).
	ValidateConfig(with ir.RawConfig) error

	// Launch runs the agent inside the supplied handle. The dispatcher
	// (slice 5.2) calls this once per attempt (one per gate attempt; one
	// per retry). Returns synchronously with the typed result AFTER the
	// agent exits and ALL events have been drained from the returned
	// channel.
	//
	// The returned channel is buffered and closed by Launch before
	// returning; callers MUST drain it (the dispatcher's slice 5.2
	// goroutine consumes events as they arrive and writes them to the log
	// as agent.event entries). On non-nil error, the returned channel is
	// nil; callers must check err before ranging.
	//
	// Error contract:
	//   - nil error + AgentResult.Output validates against inv.OutputSchema
	//     → ok outcome
	//   - *ErrUnparseableOutput → retryable_failure (transient parse miss)
	//   - *ErrAgentLaunch → retryable_failure (transport/launch issue)
	//   - *ErrInvalidConfig → permanent_failure (config bug; retry won't fix)
	//   - any other non-nil error → retryable_failure (treated as transport)
	//
	// Independence (spec §5.5): Launch MUST NOT reference, store, or
	// reuse any state from a prior Launch call against the same Adapter
	// instance. Each call is fresh.
	Launch(ctx context.Context, handle container.Handle, inv AgentInvocation) (AgentResult, <-chan AgentEvent, error)
}

// Resolver is the read-only subset of Registry. The engine's dispatcher
// (slice 5.2) takes a Resolver, not a *Registry, so the dispatcher cannot
// Register new adapters mid-run (CLAUDE.md "interpreter is the only writer
// to state" — the registry is fixed at CLI start-time per Phase 5 decision
// 3 / 17). cli/run.go and cli/resume.go (this slice) work with *Registry;
// engine internals work with Resolver.
type Resolver interface {
	// Lookup returns the Adapter registered under ref (true) or (nil, false)
	// if no such adapter is registered. Concurrency-safe; multiple goroutines
	// may Lookup concurrently (Phase 3 parallel branches dispatch concurrently
	// after Phase 5 wires AgentStep).
	Lookup(ref string) (Adapter, bool)
}
