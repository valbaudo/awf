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
func runAgentStep(
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

	// Slice 5.3: populate Feedback from the enclosing gate's prior verdict.
	// When `path` is inside a gate's `.generate.` subtree, the same gate-path
	// resolver used by template `{{ evaluate.<field> }}` (engine/gate_path.go)
	// gives us the gate's path; from runstate.LookupGateAttempts we read the
	// most-recent committed verdict. nil on attempt 1 (no prior verdict yet),
	// nil for non-gate paths.
	//
	// Anti-aliasing note: the map copy below is INTENTIONAL — `last.Verdict`
	// is a pointer to the live runstate.GateAttempts map. Aliasing it into
	// `feedback` would let downstream callers (the adapter, a future template
	// substitution path) mutate runstate by mistake. The copy keeps Feedback
	// owned by this step's invocation, runstate immutable. Cheap — verdicts
	// are small (typed schema-validated outputs, ~10-100 fields).
	var feedback ir.RawConfig
	if gatePath, ok := enclosingGateForEvaluate(path); ok {
		attempts := runstate.LookupGateAttempts(gatePath)
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			if len(last.Verdict) > 0 {
				feedback = ir.RawConfig{}
				for k, v := range last.Verdict {
					feedback[k] = v
				}
			}
		}
	}

	// continues: threading — assemble the prior turns from the committed log.
	// Pure over the committed log: StepPathIndex is a deterministic whole-graph
	// walk; stepRuntimePath resolves each predecessor to THIS consumer's own
	// attempt/item/iter (ctxPath == path), which is why rejected gate attempts and
	// foreign map items are excluded by addressing, not special-casing. One Scope
	// is reused across the walk. Walked root->current (prepend), so the oldest
	// turn is Thread[0] and the immediate predecessor is last.
	//
	// Phase 5.1: hoist StepPathIndex/threadTargets to once-per-run (requires a
	// runAgentStep signature change). The per-step walk is correct and cheap for
	// AWF's bounded single-host graphs.
	var thread []agent.ThreadTurn
	idx := StepPathIndex(wf)
	threadScope := NewScope(runstate, wf, path)
	for cur := as.Continues; cur != ""; {
		predRuntime, perr := threadScope.stepRuntimePath(idx[cur])
		if perr != nil { // impossible after validation (AWF1026/AWF1030); defensive.
			return failStep(log, path, OutcomePermanentFailure,
				fmt.Errorf("engine.runAgentStep: resolve continues target %q at %q: %w", cur, path, perr))
		}
		predNR, ok := runstate.LookupCompleted(predRuntime)
		if !ok { // ok guaranteed by dominance (AWF1026); defensive.
			return failStep(log, path, OutcomePermanentFailure,
				fmt.Errorf("engine.runAgentStep: continues target %q not committed (runtime %q)", cur, predRuntime))
		}
		thread = append([]agent.ThreadTurn{{User: predNR.Transcript.User, Assistant: predNR.Transcript.Assistant}}, thread...)
		cur = continuesOf(wf, cur)
	}

	// 4. Build ResolvedInputs. Timeout cast follows the runCodeStep idiom
	// (engine/interpreter.go:283): ir.AgentStep.Timeout is *ir.Duration where
	// `type Duration time.Duration`, so the deref-then-cast is the conversion.
	snapBare, _ := SplitContainerRef(as.Container)
	resolved := ResolvedInputs{
		Uses:                  as.Uses,
		With:                  resolvedWith,
		OutputSchema:          as.OutputSchema,
		NonRetryableExitCodes: policy.NonRetryableExitCodes,
		Snapshot:              wf.Containers[snapBare].Snapshot,
		Feedback:              feedback, // slice 5.3
		Thread:                thread,   // Task 4.5
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

	appendNodeStarted(log, path, "agent")

	dr, chunks, runErr := RunWithRetry(ctx, dispatcher, intent, policy, clk, log)
	// Drain via the canonical helper. Agent steps' chunks channel is the
	// pre-closed one runAgent returns, so this is a no-op on the agent path,
	// but using drainTap keeps the dispatch tail symmetric with runCodeStep
	// (engine/interpreter.go:298) and inherits its defensive nil-handling +
	// tap-write-failure suppression.
	drainTap(chunks, as.ID, tap)

	// 4. Write agent.event log entries from the buffered events. Done BEFORE
	// the commit so the journal records them adjacent to the node.completed
	// (happy path) or node.failed (failure path). On failure, runErr is
	// authoritative — appendErr is reported only when runErr is nil so we
	// never silently mask a step failure with an internal append error.
	appendErr := appendAgentEvents(log, blobs, path, dr.AgentEvents)

	// 5. Failure paths: mirror runCodeStep's split (engine/interpreter.go:309-316).
	// dr.Outcome == "" means the dispatcher never dispatched (unknown container,
	// ErrUnsupportedKind). That's an INTERNAL error — no node.failed entry,
	// no fold corruption on resume. dr.Outcome != "" means the step ran and
	// failed — failStep writes node.failed with the underlying cause.
	if runErr != nil {
		if dr.Outcome == "" {
			return "", fmt.Errorf("engine.runAgentStep: dispatch at path %q: %w", path, runErr)
		}
		return failStep(log, path, dr.Outcome, runErr)
	}

	// On happy path, surface any earlier appendAgentEvents failure now.
	if appendErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: append agent.event entries at %q: %w", path, appendErr)
	}

	// 6. Happy path: commit via the canonical engine.Commit. Commit owns the
	// content-address-then-pointer-swap invariant (CLAUDE.md "Commit"); we
	// reuse it verbatim. Then mirror the result into runstate. A step
	// participates in a conversation (so its transcript blob must be committed)
	// iff it continues someone OR is continued-from by some other step.
	// Phase 5.1: hoist threadTargets to once-per-run.
	participates := as.Continues != "" || threadTargets(wf)[as.ID]
	nr, commitErr := Commit(log, blobs, path, dr, participates)
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
func substituteRawConfig(in ir.RawConfig, scope template.Scope) (ir.RawConfig, error) {
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

// continuesOf returns the Continues target of the agent step with the given id,
// or "" if the id is not an agent step (cannot happen after AWF1025 validation).
// Used to walk a continues: chain to its root during thread assembly. Built on
// ir.WalkNodes (the single whole-graph pre-order walk) so it can never drift
// from the runtime addressing the rest of the engine uses.
func continuesOf(wf *ir.Workflow, id string) string {
	var out string
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, _ string) {
		if as, ok := n.(*ir.AgentStep); ok && as.ID == id {
			out = as.Continues
		}
	})
	return out
}

// threadTargets returns the set of step ids that appear as some agent step's
// continues: target. A step participates in a conversation (and so commits its
// transcript blob) iff it has a continues: OR is some other step's target.
// Computed per agent-step entry; Phase 5.1 hoists it to run scope.
func threadTargets(wf *ir.Workflow) map[string]bool {
	tt := map[string]bool{}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, _ string) {
		if as, ok := n.(*ir.AgentStep); ok && as.Continues != "" {
			tt[as.Continues] = true
		}
	})
	return tt
}

// appendAgentEvents writes one agent.event log entry per buffered AgentEvent.
// Payloads ≥ agentEventInlineThreshold are offloaded to Blobs and the entry
// carries the CAS pointer; smaller payloads are inline.
func appendAgentEvents(log state.Log, blobs state.Blobs, path string, events []agent.AgentEvent) error {
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
