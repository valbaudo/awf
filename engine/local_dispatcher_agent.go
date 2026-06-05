// engine/local_dispatcher_agent.go — the AgentStep dispatch path. Symmetric
// to runCode (in local_dispatcher.go); separated because the agent path is
// self-contained (~150 LOC) and the two paths don't share helpers. See the
// LocalDispatcher.Run kind-switch in local_dispatcher.go for the entry point.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// runAgent is the agent-step counterpart to runCode. Symmetric in shape;
// returns DispatchResult with one of the outcomes listed above.
//
// Outcome mapping (spec §6 mechanical only):
//   - clean Launch + valid Output → ok
//   - *agent.ErrAdapterNotFound      → internal halt (returned as Run's err
//     with dr.Outcome=""; runAgentStep's dr.Outcome=="" branch returns
//     ("", err) — NO node.failed event; preserves fold integrity)
//   - *agent.ErrInvalidConfig         → permanent_failure (workflow bug)
//   - *agent.ErrUnparseableOutput     → retryable_failure (parse miss)
//   - *agent.ErrAgentLaunch / other  → retryable_failure (transport class)
//   - adapter.Refused (slice 5.3+)   → permanent_failure (policy block)
func (d *LocalDispatcher) runAgent(ctx context.Context, intent NodeIntent, as *ir.AgentStep) (DispatchResult, <-chan container.IOChunk, error) {
	// Defense-in-depth nil check. The conformance harness (Task 12) and the
	// CLI (Task 11) always initialize Resolver to a non-nil empty Registry,
	// so this branch normally cannot fire. It exists for callers that
	// construct LocalDispatcher{} directly without setting Resolver — most
	// code-step-only unit tests in engine/local_dispatcher_test.go do — and
	// for the typed-nil interface gotcha (https://go.dev/doc/faq#nil_error):
	// `d.Resolver == nil` catches only an UNTYPED nil. If a caller assigns
	// a typed `(*agent.Registry)(nil)` to the interface, the check passes
	// false and Lookup panics. Harness/CLI initialization defuses that,
	// but the explicit untyped-nil branch here keeps the code-step path
	// safe regardless of caller hygiene.
	if d.Resolver == nil {
		return DispatchResult{}, nil, &agent.ErrAdapterNotFound{Ref: intent.ResolvedInputs.Uses}
	}
	adapter, ok := d.Resolver.Lookup(intent.ResolvedInputs.Uses)
	if !ok {
		return DispatchResult{}, nil, &agent.ErrAdapterNotFound{Ref: intent.ResolvedInputs.Uses}
	}

	// Resolve container handle (same lookup runCode does). A containerless
	// agent step (empty ref — permitted for Containerless adapters, gated at
	// run-start in cli/runtimes.go) gets the zero Handle; the adapter ignores
	// it (e.g. awf/llm issues a direct HTTP call).
	bare, svcOverride := SplitContainerRef(as.Container)
	var h container.Handle
	if bare != "" {
		var ok bool
		h, ok = d.Handles[bare]
		if !ok {
			return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher.runAgent: no handle for container %q (bare %q) at path %q", as.Container, bare, intent.Path)
		}
		if svcOverride != "" {
			h.Service = svcOverride
		}
	}

	// Apply step timeout to ctx (mirrors runCode).
	if intent.ResolvedInputs.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, intent.ResolvedInputs.Timeout)
		defer cancel()
	}

	// Validate config defensively (the interpreter-level engine/agent_step.go
	// also validates at run start — this is double-checked at dispatch time per
	// the Phase 5 design slice 5.2 row "Calls Adapter.ValidateConfig (defensively)").
	if err := adapter.ValidateConfig(intent.ResolvedInputs.With); err != nil {
		// Permanent failure: bad config won't fix itself on retry.
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     err,
		}, nil, nil
	}

	// Build AgentInvocation. Feedback (slice 5.3) carries the prior evaluator
	// verdict on gate repair attempts N>1 — populated by runAgentStep from
	// the enclosing gate's runstate.LookupGateAttempts. Adapters that consume
	// an implicit "previous verdict:" preamble (the Claude Code adapter) read
	// it; nil on attempt 1, non-gate paths, and code steps.
	inv := agent.AgentInvocation{
		NodePath:       intent.Path,
		Uses:           intent.ResolvedInputs.Uses,
		With:           intent.ResolvedInputs.With,
		OutputSchema:   intent.ResolvedInputs.OutputSchema,
		IdempotencyKey: intent.IdempotencyKey,
		Feedback:       intent.ResolvedInputs.Feedback, // slice 5.3
	}

	// γ contract: Launch returns immediately with events + outcome channels
	// open. Spawn a goroutine that drains events (writing tap lines per event
	// as they arrive — THE REALTIME UX). Main path awaits launchOutcome.
	//
	// Naming: dispatchOutcome (engine's mechanical Outcome enum) vs
	// launchOutcome (γ's AgentOutcome envelope). Two different types — keep
	// names disambiguated.
	events, outcomeCh, launchErr := adapter.Launch(ctx, h, inv)
	if launchErr != nil {
		dispatchOutcome := classifyAgentLaunchErr(launchErr)
		return DispatchResult{Outcome: dispatchOutcome, Err: launchErr}, nil, nil
	}

	drainDone := make(chan []agent.AgentEvent, 1)
	go func() {
		var buf []agent.AgentEvent
		render := d.eventRenderer()
		for ev := range events {
			if d.AgentEventTap != nil {
				render(d.AgentEventTap, ev)
			}
			ev.Display = agent.EventDisplay{} // transient; not journaled — drop before buffering
			buf = append(buf, ev)
		}
		drainDone <- buf
	}()

	launchOutcome := <-outcomeCh
	bufferedEvents := <-drainDone

	// In-flight failure surfaced via launchOutcome.Err.
	if launchOutcome.Err != nil {
		dispatchOutcome := classifyAgentLaunchErr(launchOutcome.Err)
		return DispatchResult{
			Outcome:     dispatchOutcome,
			Err:         launchOutcome.Err,
			AgentEvents: bufferedEvents,
		}, closedChunks(), nil
	}

	// Validate Output against OutputSchema (if declared).
	if intent.ResolvedInputs.OutputSchema != nil {
		if err := ValidateOutputMap(launchOutcome.Result.Output, intent.ResolvedInputs.OutputSchema); err != nil {
			return DispatchResult{
				Outcome:     OutcomeRetryableFailure,
				Outputs:     launchOutcome.Result.Output,
				AgentEvents: bufferedEvents,
				Err:         &agent.ErrUnparseableOutput{NodePath: intent.Path},
			}, closedChunks(), nil
		}
	}

	exitCodePtr := copyIntPtr(launchOutcome.Result.ExitCode)
	metrics := launchOutcome.Result.Metrics
	if d.StepCostLine && d.AgentEventTap != nil {
		writeAgentCostLine(d.AgentEventTap, intent.Path, metrics)
	}
	dr := DispatchResult{
		Outcome:     OutcomeOK,
		ExitCode:    exitCodePtr,
		Outputs:     launchOutcome.Result.Output,
		AgentEvents: bufferedEvents,
		Files:       packFiles(launchOutcome.Result.Files),
		Metrics:     &metrics,
	}

	// Record the container for OK (committed) steps; failed steps carry none
	// (decision 3: node.completed.container is recorded only for committed/OK
	// steps — obs reads it off every committed step's node.completed).
	if dr.Outcome == OutcomeOK {
		dr.Container = bare
	}
	// snapshot:workspace capture (slice 7.1) — identical to runCode. Only an OK
	// step that committed records its container; a failed launch returned
	// earlier without it. Capture the CoW workspace diff after success;
	// terminal failure → permanent_failure (Phase-4 decision 11), transient →
	// retryable, classified behind the container seam via errors.Is.
	if dr.Outcome == OutcomeOK && intent.ResolvedInputs.Snapshot == "workspace" {
		ref, snapErr := d.Backend.Snapshot(ctx, h)
		if snapErr != nil {
			oc := snapshotFailureOutcome(snapErr)
			return DispatchResult{
				Outcome:     oc,
				ExitCode:    exitCodePtr,
				AgentEvents: bufferedEvents,
				Err:         fmt.Errorf("engine.LocalDispatcher.runAgent: snapshot %q at %q: %w", bare, intent.Path, snapErr),
			}, closedChunks(), nil
		}
		dr.SnapshotRef = string(ref)
	}
	return dr, closedChunks(), nil
}

// classifyAgentLaunchErr maps an Adapter.Launch error to an Outcome per the
// Adapter contract (agent/adapter.go doc on Launch). Defaults to
// retryable_failure for transport-class faults.
func classifyAgentLaunchErr(err error) Outcome {
	var notFound *agent.ErrAdapterNotFound
	var badConfig *agent.ErrInvalidConfig
	var unparseable *agent.ErrUnparseableOutput
	switch {
	// Defense-in-depth: *ErrAdapterNotFound is normally caught before Launch by
	// the Resolver.Lookup miss path in runAgent. This branch handles the unlikely
	// case of an adapter whose Launch surfaces a wrapped *ErrAdapterNotFound,
	// violating the agent/adapter.go Launch contract. Classification still maps
	// to permanent_failure — a broken adapter contract is not transient.
	case errors.As(err, &notFound):
		return OutcomePermanentFailure
	case errors.As(err, &badConfig):
		return OutcomePermanentFailure
	case errors.As(err, &unparseable):
		return OutcomeRetryableFailure
	default:
		// *agent.ErrAgentLaunch and any other error class → transport.
		return OutcomeRetryableFailure
	}
}

// closedChunks returns a pre-closed IOChunk channel — runAgent doesn't emit
// IOChunks (agent events are a separate stream), but the Dispatcher.Run
// contract guarantees a non-nil channel on a successful return so callers
// can `for range` safely without nil-checking.
func closedChunks() <-chan container.IOChunk {
	ch := make(chan container.IOChunk)
	close(ch)
	return ch
}

// packFiles converts agent.AgentResult.Files (map[path][]byte) into the
// container.CapturedFile shape DispatchResult.Files expects.
func packFiles(files map[string][]byte) []container.CapturedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]container.CapturedFile, 0, len(files))
	for path, content := range files {
		out = append(out, container.CapturedFile{Path: path, Content: content})
	}
	return out
}

// eventRenderer returns the live-tap formatter: the injected RenderAgentEvent if
// set, else the built-in terse fallback. Never nil.
func (d *LocalDispatcher) eventRenderer() func(io.Writer, agent.AgentEvent) {
	if d.RenderAgentEvent != nil {
		return d.RenderAgentEvent
	}
	return writeAgentEventTap
}

// writeAgentCostLine emits one per-step cost/token summary line to the live tap.
// Best-effort (write errors ignored, like writeAgentEventTap). Sourced from the
// adapter's MetricSet — no harness parsing, no obs.
func writeAgentCostLine(w io.Writer, path string, m agent.MetricSet) {
	_, _ = fmt.Fprintf(w, "%s %s — $%.4f · %d tok (%d in / %d out) · %d turns\n",
		"·", path, m.Cost.USD, m.Tokens.Input+m.Tokens.Output, m.Tokens.Input, m.Tokens.Output, m.Turns)
}

// writeAgentEventTap emits one line per AgentEvent to the tap writer.
// Write errors are intentionally ignored — the tap is best-effort stderr
// output; a write failure must not abort the dispatch attempt.
func writeAgentEventTap(w io.Writer, ev agent.AgentEvent) {
	const maxPayloadPreview = 80
	preview := ev.Payload
	if len(preview) > maxPayloadPreview {
		preview = preview[:maxPayloadPreview]
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", ev.Kind, preview)
}
