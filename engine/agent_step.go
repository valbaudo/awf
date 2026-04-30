package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// runAgentStep is the interpreter-side handler for *ir.AgentStep — symmetric
// to runCodeStep. It:
//
//  1. Builds a template.Scope from RunState + the step's runtime path. This
//     same scope is what resolves {{ evaluate.<field> }} when the step sits
//     inside a gate's generate block (Phase 3.3 wiring; see baseline note
//     in the plan's Task 8 prose).
//  2. Substitutes string-leaf values in AgentStep.With via template.Substitute.
//  3. Substitutes AgentStep.IdempotencyKey (if any).
//  4. Builds NodeIntent and calls engine.RunWithRetry.
//  5. Writes one agent.event log entry per buffered AgentEvent (Blobs offload
//     for payloads ≥ agentEventInlineThreshold).
//  6. Calls the canonical engine.Commit (engine/commit.go) and records the
//     resulting NodeResult in runstate.
//
// Per CLAUDE.md "interpreter is the only writer to state": Log.Append calls
// are made HERE (and via the canonical Commit), not in the dispatcher. Per
// "simplest solution first": the commit logic is NOT duplicated — slice 5.2
// reuses engine.Commit verbatim.
func runAgentStep( //nolint:unused // wired in Task 9 (interpNodes switch case *ir.AgentStep)
	ctx context.Context,
	as *ir.AgentStep,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	_ *signal.Broker,
) (Outcome, error) {
	// Resume short-circuit (mirrors runCodeStep — committed nodes are replayed,
	// not recomputed).
	if _, ok := runstate.LookupCompleted(path); ok {
		return OutcomeOK, nil
	}

	scope := NewScope(runstate, wf, path)

	// 1. Substitute With.
	resolvedWith, err := substituteRawConfig(as.With, scope)
	if err != nil {
		return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.runAgentStep: substitute with at %q: %w", path, err))
	}

	// 2. Substitute idempotency_key (if any).
	var idempotencyKey string
	if as.IdempotencyKey != nil {
		idempotencyKey, err = template.Substitute(string(*as.IdempotencyKey), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.runAgentStep: substitute idempotency_key at %q: %w", path, err))
		}
	}

	// 3. Build retry policy. Verified pattern from runCodeStep
	// (engine/interpreter.go:271): retry.Merge returns (retry.Policy, error);
	// an error here is an author bug in the workflow's retry: block.
	policy, err := retry.Merge(retry.Default, as.Retry)
	if err != nil {
		return "", fmt.Errorf("engine.runAgentStep: build retry policy at path %q: %w", path, err)
	}

	// 4. Build ResolvedInputs. Timeout cast follows the runCodeStep idiom
	// (engine/interpreter.go:283): ir.AgentStep.Timeout is *ir.Duration where
	// `type Duration time.Duration`, so the deref-then-cast is the conversion.
	resolved := ResolvedInputs{
		Uses:                  as.Uses,
		With:                  resolvedWith,
		OutputSchema:          as.OutputSchema,
		NonRetryableExitCodes: policy.NonRetryableExitCodes,
	}
	if as.Timeout != nil {
		resolved.Timeout = time.Duration(*as.Timeout)
	}

	intent := NodeIntent{
		Path:           path,
		Node:           as,
		ResolvedInputs: resolved,
		IdempotencyKey: idempotencyKey,
	}

	dr, chunks, runErr := RunWithRetry(ctx, dispatcher, intent, policy, clk, log)
	// Drain via the canonical helper. Agent steps' chunks channel is the
	// pre-closed one runAgent returns, so this is a no-op on the agent path,
	// but using drainTap keeps the dispatch tail symmetric with runCodeStep
	// (engine/interpreter.go:298) and inherits its defensive nil-handling +
	// tap-write-failure suppression.
	drainTap(chunks, as.ID, tap)

	// 4. Write agent.event log entries from the buffered events. Done BEFORE
	// the commit so the journal records them adjacent to the node.completed.
	if appendErr := appendAgentEvents(log, blobs, path, dr.AgentEvents); appendErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: append agent.event entries at %q: %w", path, appendErr)
	}

	// 5. Failure paths: classify + commit node.failed via the existing helper.
	if runErr != nil {
		outcome := dr.Outcome
		if outcome == "" {
			outcome = OutcomeRetryableFailure
		}
		return failStep(log, path, outcome, runErr)
	}

	// 6. Happy path: commit via the canonical engine.Commit. Commit owns the
	// content-address-then-pointer-swap invariant (CLAUDE.md "Commit"); we
	// reuse it verbatim. Then mirror the result into runstate.
	nr, commitErr := Commit(log, blobs, path, dr)
	if commitErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: commit at %q: %w", path, commitErr)
	}
	runstate.RecordCompleted(path, nr)
	return OutcomeOK, nil
}

// substituteRawConfig walks the With map and applies template.Substitute to
// every string leaf. Non-string values (numbers, booleans, nested objects)
// are not substituted in slice 5.2 — only top-level string fields (matching
// what AWF spec §7 templating supports: "substitution = fill references
// before a command runs"). Nested-object substitution lands when a real
// adapter demands it.
//
// Returns a freshly-allocated map; never mutates the input.
func substituteRawConfig(in ir.RawConfig, scope template.Scope) (ir.RawConfig, error) { //nolint:unused // called by runAgentStep, wired in Task 9
	if len(in) == 0 {
		return nil, nil
	}
	out := make(ir.RawConfig, len(in))
	for k, v := range in {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		sub, err := template.Substitute(s, scope)
		if err != nil {
			return nil, fmt.Errorf("substitute %q: %w", k, err)
		}
		out[k] = sub
	}
	return out, nil
}

// appendAgentEvents writes one agent.event log entry per buffered AgentEvent.
// Payloads ≥ agentEventInlineThreshold are offloaded to Blobs and the entry
// carries the CAS pointer; smaller payloads are inline.
func appendAgentEvents(log state.Log, blobs state.Blobs, path string, events []agent.AgentEvent) error { //nolint:unused // called by runAgentStep, wired in Task 9
	for _, ev := range events {
		data := AgentEventData{
			Kind:   ev.Kind,
			Stream: ev.Stream,
			Size:   len(ev.Payload),
		}
		if len(ev.Payload) >= agentEventInlineThreshold {
			ref, err := blobs.Put(ev.Payload)
			if err != nil {
				return fmt.Errorf("Blobs.Put agent.event payload: %w", err)
			}
			data.PayloadRef = ref
		} else {
			data.PayloadInline = ev.Payload
		}
		payload, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal AgentEventData: %w", err)
		}
		if err := log.Append(state.Event{Type: EventAgentEvent, Path: path, Data: payload}); err != nil {
			return fmt.Errorf("Log.Append agent.event: %w", err)
		}
	}
	return nil
}
