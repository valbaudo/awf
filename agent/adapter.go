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
// Phase 5 design decision 7: adapters with Caps.PersistentSession == false
// MUST NOT reuse sessions across calls. Each Launch is a fresh context — for
// Claude Code this means no --continue / --resume / --session-id flags;
// ValidateConfig rejects any with-key that would enable session reuse.
//
// Adapters with Caps.PersistentSession == true may reuse provider-owned state
// only through explicit adapter-owned configuration (for example with.session);
// the core still treats `with:` as opaque. PR0a CLI and defensive engine
// guards reject PersistentSession adapters in gate.evaluate so the gate's
// independence property (spec §5.5) remains engine-enforced.
//
// Phase 5 design decision 16: Capabilities() reports static claims about
// the adapter's typed-output pipeline and safety properties. The conformance
// suite routes adapters to the right buckets, and run-start plus defensive
// guards use safety caps to fail closed before Launch.
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

	// Launch runs the agent inside the supplied handle. Slice 5.3 γ contract:
	// returns IMMEDIATELY with both channels open. The events channel emits
	// AgentEvents as the harness produces them (one per content block for
	// assistant messages); the caller MUST range over it concurrently with
	// awaiting outcome. The events channel CLOSES when the harness's stream
	// ends (process exit + stdout pipe drain). The outcome channel delivers
	// exactly one AgentOutcome AFTER events closes, then closes itself.
	//
	// === DRAIN-OR-LEAK CONTRACT =====================================
	// Callers MUST drain the events channel to completion. Failure to
	// drain blocks the adapter's parser goroutine on `events <- ev` once
	// the channel's internal buffer fills (Claude adapter: 16 events;
	// fake adapter: scripted-event-count + 1, so unbounded for practical
	// purposes). A blocked parser goroutine cannot send AgentOutcome on
	// outcomeCh — it hangs until ctx-cancel. Two safe patterns:
	//
	//   // Sequential drain (simplest):
	//   events, outcomeCh, err := adapter.Launch(...)
	//   if err != nil { /* pre-launch failure */ }
	//   for ev := range events { /* render, log, etc. */ }
	//   outcome := <-outcomeCh
	//
	//   // Concurrent drain (runAgent's pattern — preserves realtime UX):
	//   events, outcomeCh, err := adapter.Launch(...)
	//   if err != nil { /* pre-launch failure */ }
	//   drainDone := make(chan []AgentEvent, 1)
	//   go func() {
	//       var buf []AgentEvent
	//       for ev := range events {
	//           buf = append(buf, ev)
	//           // tap.Write(ev) here — this is where realtime renders.
	//       }
	//       drainDone <- buf
	//   }()
	//   outcome := <-outcomeCh
	//   buf := <-drainDone
	//
	// Adapters that buffer events to a size matching scripted/expected
	// event count (the fake's pattern) are forgiving — caller-side bugs
	// that ignore events don't leak. The Claude adapter cannot do this
	// because event count is unknown until claude exits, so it ships a
	// fixed-size buffer and relies on the caller to drain.
	// ================================================================
	//
	// On a non-nil err return (pre-launch), BOTH channels are nil — the
	// adapter never reached the launch step. On a successful return both
	// channels are non-nil and must be drained.
	//
	// Independence (spec §5.5): when Capabilities().PersistentSession is false,
	// Launch MUST NOT reference, store, or reuse any state from a prior Launch
	// call against the same Adapter instance. When PersistentSession is true,
	// any reuse must be explicit adapter-owned provider state (for example
	// with.session), and PR0a guards prevent such adapters from running in
	// gate.evaluate.
	//
	// (Pre-slice-5.3: signature was (AgentResult, <-chan AgentEvent,
	// error) with events pre-closed before Launch returned — buffer-then-
	// burst. The γ rewrite enables true realtime UX; the events
	// buffer-then-burst pattern is preserved by the caller's
	// `for range events` loop.)
	Launch(ctx context.Context, handle container.Handle, inv AgentInvocation) (<-chan AgentEvent, <-chan AgentOutcome, error)
}

// ResumePreflighter is implemented only by adapters whose Capabilities report
// PersistentSession. The CLI calls it on resume after folding the AWF log and
// before appending run.resumed, giving the adapter a chance to reject unsafe
// live replay without mutating AWF state.
type ResumePreflighter interface {
	PreflightResume(context.Context, LiveResumePreflightRequest) error
}

// ToolLoopRunner is implemented only by adapters that can run an engine-mediated
// tool loop (Caps.Containerless && Caps.Threaded — in v1, only awf/llm). It is an
// OPTIONAL interface (the ResumePreflighter pattern), NOT part of the Adapter seam,
// so the other four adapters are untouched. runReact obtains it via a type assertion.
type ToolLoopRunner interface {
	RunToolLoop(ctx context.Context, inv ToolLoopInvocation) (ToolLoopResult, error)
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
