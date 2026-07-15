package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// runSignalStep is the await-step handler (spec §4.3 + Phase 3 design §F).
//
// Flow:
//
//  1. Resume-check: if path ∈ runstate.Completed, skip (replayed-not-recomputed
//     per spec §8).
//  2. Nil-broker defense.
//  3. Half-commit-resume check (Design Q7): if path ∈ runstate.SignalReceivedAt,
//     the prior run committed signal.received but not node.completed. Use the
//     stored {Seq, PayloadRef}; SKIP the broker.Receive call AND the
//     signal.received Append; jump straight to step 7 (node.completed).
//  4. broker.Receive(ctx, await-name, timeout). Per spec §4.3:
//     timeout → retryable_failure; ctx-cancel → propagate (without failStep if
//     operator-initiated, per M7).
//  5. Validate payload against output_schema if declared. Empty payload + schema
//     declared → ValidateAgainstSchema naturally fails ("empty input") →
//     retryable_failure.
//  6. Canonicalize the validated typed map via json.Marshal BEFORE CAS (M4) —
//     CAS determinism across retries. If no schema, CAS the raw bytes as-is.
//  7. Atomic commit: Blobs.Put → signal.received Append + Sync → node.completed
//     Append + Sync → RunState mirrors.
func runSignalStep(
	ctx context.Context,
	ss *ir.SignalStep,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	_ Dispatcher,
	log state.Log,
	blobs state.Blobs,
	_ clock.Clock,
	_ io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	return runSignalStepWithContext(ctx, ss, path, interpreterContext{
		wf: wf, runstate: runstate, dispatcher: nil, log: log, blobs: blobs, broker: broker,
	})
}

func runSignalStepWithContext(
	ctx context.Context,
	ss *ir.SignalStep,
	path string,
	ictx interpreterContext,
) (Outcome, error) {
	// 1. Resume-check.
	if _, done := ictx.runstate.LookupCompleted(path); done {
		return OutcomeOK, nil
	}

	// 2. Nil-broker defense.
	if ictx.broker == nil {
		return "", fmt.Errorf("engine.runSignalStep: signal step %q reached but engine.Run was called with nil *signal.Broker — CLI/conformance must construct a broker before invoking the engine on a workflow with signal steps", path)
	}

	// 3. Half-commit-resume check (Design Q7 + C6 refinement). If
	//    signal.received was journaled in a prior run but node.completed
	//    didn't make it, reuse the stored ref. The handler skips broker.Receive
	//    AND the signal.received Append (avoiding the duplicate-event bug
	//    critique C3 identified).
	//
	//    Payload is RE-DERIVED via Blobs.Get + ValidateAgainstSchema (C6 fix):
	//    refs-only in SignalReceivedEntry to support non-object payloads from
	//    unschema'd signals. Re-validation is cheap and idempotent — spec §8
	//    pinning guarantees the schema is identical across resume, so round-1's
	//    validation success implies round-2's will too.
	var payloadRef string
	var typedOutputs map[string]any
	if entry, ok := ictx.runstate.LookupSignalReceivedAt(path); ok {
		payloadRef = entry.PayloadRef
		// Re-derive typed payload from CAS if schema declared.
		if ss.OutputSchema != nil && payloadRef != "" {
			raw, gerr := ictx.blobs.Get(payloadRef)
			if gerr != nil {
				return "", fmt.Errorf("engine.runSignalStep: read half-commit payload at %q: %w", path, gerr)
			}
			validated, verr := ValidateAgainstSchema(raw, ss.OutputSchema)
			if verr != nil {
				// Should not happen — round 1 validated. If it does, the blob
				// or schema is corrupt; surface as internal error.
				return "", fmt.Errorf("engine.runSignalStep: half-commit payload re-validation at %q: %w", path, verr)
			}
			typedOutputs = validated
		}
		// Skip broker.Receive AND signal.received Append (already journaled);
		// jump to node.completed at step 7b below.
	} else {
		appendNodeStarted(ictx.log, path, "signal")

		// 4. Block on broker.Receive.
		timeout := time.Duration(0)
		if ss.Timeout != nil {
			timeout = time.Duration(*ss.Timeout)
		}
		// Keyed consumption (spec §3.5 C5, grammar rewritten by F18). With
		// ss.Where, build a payload predicate: parse the where: clause's ONE
		// `{{ }}` envelope ONCE via the SAME bounded-boolean evaluator
		// if:/until: use (template.ParseExpr — no template.Substitute, no
		// string synthesis). The returned MatchFunc evaluates that SAME parsed
		// expr against a signalScope for each candidate payload: `signal.*`
		// resolves against that candidate's payloadScope, every other root
		// (input.*, step.*, run.*, <as>.*, …) resolves against the constant
		// engine scope. Because a value is compared as DATA — never spliced
		// into a string that gets re-parsed — neither a payload field nor an
		// outer ref's value can alter the predicate's grammar (closes the
		// injection class the old substitute-then-parse path opened). The
		// earliest matching signal is consumed; non-matching signals stay
		// buffered.
		var (
			d    signal.Delivery
			rerr error
		)
		if ss.Where != "" {
			match, perr := buildWherePredicateWithScope(ss.Where, ictx.scope(path))
			if perr != nil {
				// Either the grammar failed to parse (an AWF1036 the validator
				// should have caught) OR an OUTER ref (input.*/step.*/…) failed
				// to resolve against the engine scope. Both are author bugs
				// surfaced synchronously BEFORE polling the broker — permanent,
				// since re-running is identical. (signal.* refs are the ONLY
				// deferred part: a missing payload field is a real per-candidate
				// non-match handled inside the MatchFunc, not a build error.)
				return failStep(ictx.log, path, OutcomePermanentFailure,
					fmt.Errorf("signal %q where: %w", ss.Await, perr))
			}
			d, rerr = ictx.broker.ReceiveMatching(ctx, ss.Await, timeout, match)
		} else {
			d, rerr = ictx.broker.Receive(ctx, ss.Await, timeout)
		}
		if rerr != nil {
			if errors.Is(rerr, context.DeadlineExceeded) {
				return failStep(ictx.log, path, OutcomeRetryableFailure,
					fmt.Errorf("signal %q await timeout (%s)", ss.Await, timeout))
			}
			// M7 fix: ctx.Canceled caused by the operator (cancel.json /
			// pause.json detected by pollControls) is NOT a step failure.
			// engine.Run will append the terminal run.cancelled / run.paused
			// event after this handler returns. Don't emit node.failed —
			// that'd create a redundant terminal record + block resume on
			// cli/resume.go's node.failed refusal class.
			if ictx.runstate.IsCancelled() || ictx.runstate.LookupPaused() != nil {
				return "", rerr
			}
			if errors.Is(rerr, context.Canceled) {
				// True Ctrl-C cancellation — emit node.failed.
				return failStep(ictx.log, path, OutcomeRetryableFailure,
					fmt.Errorf("signal %q await: %w", ss.Await, rerr))
			}
			// Broker filesystem failures are AWF infrastructure errors. They are
			// not a missing signal and must not be journaled as a retryable step.
			return "", fmt.Errorf("engine.runSignalStep: signal %q broker: %w", ss.Await, rerr)
		}

		// 5. Validate payload against output_schema if declared. Empty payload
		//    + schema declared → ValidateAgainstSchema rejects empty input
		//    naturally (engine/schema.go:31-33). No separate len(payload) > 0
		//    guard (C5 fix).
		if ss.OutputSchema != nil {
			validated, verr := ValidateAgainstSchema(d.Payload, ss.OutputSchema)
			if verr != nil {
				return failStep(ictx.log, path, OutcomeRetryableFailure,
					fmt.Errorf("signal %q payload failed output_schema validation: %w", ss.Await, verr))
			}
			typedOutputs = validated
		}

		// 6. Canonicalize the validated payload (M4 fix). After validation,
		//    re-marshal the typed map to canonical JSON before CAS so the ref
		//    is deterministic across re-validation (resume re-derives via
		//    Blobs.Get + json.Unmarshal — same typed map, but the raw bytes
		//    are now the canonical form, not the operator's input formatting).
		//    Matches engine/commit.go:44 pattern.
		seq := d.Seq
		if typedOutputs != nil {
			canonicalBytes, mErr := json.Marshal(typedOutputs)
			if mErr != nil {
				return "", fmt.Errorf("engine.runSignalStep: marshal canonical payload at %q: %w", path, mErr)
			}
			ref, perr := ictx.blobs.Put(canonicalBytes)
			if perr != nil {
				return "", fmt.Errorf("engine.runSignalStep: put payload at %q: %w", path, perr)
			}
			payloadRef = ref
		} else if len(d.Payload) > 0 {
			// No schema declared → CAS the raw bytes as-is (no canonicalization
			// target; the operator's bytes are the wire form).
			ref, perr := ictx.blobs.Put(d.Payload)
			if perr != nil {
				return "", fmt.Errorf("engine.runSignalStep: put payload at %q: %w", path, perr)
			}
			payloadRef = ref
		}

		// 7a. Append signal.received + fsync.
		sigData, mErr := json.Marshal(SignalReceivedData{
			Name:       ss.Await,
			Seq:        seq,
			PayloadRef: payloadRef,
		})
		if mErr != nil {
			return "", fmt.Errorf("engine.runSignalStep: marshal signal.received at %q: %w", path, mErr)
		}
		if err := ictx.log.Append(state.Event{Type: EventSignalReceived, Path: path, Data: sigData}); err != nil {
			return "", fmt.Errorf("engine.runSignalStep: append signal.received at %q: %w", path, err)
		}
		if err := ictx.log.Sync(); err != nil {
			return "", fmt.Errorf("engine.runSignalStep: sync after signal.received at %q: %w", path, err)
		}
		// Mirror in RunState (used by Phase 6 obs + as the path-keyed lookup
		// for any future re-entry of the same path).
		ictx.runstate.AppendSignal(ss.Await, SignalEntry{Seq: seq, PayloadRef: payloadRef})
		ictx.runstate.RecordSignalReceivedAt(path, SignalReceivedEntry{
			Seq:        seq,
			PayloadRef: payloadRef,
		})
	}

	// 7b. Append node.completed + fsync. Reached by both the fresh-receive
	//     path (above) AND the half-commit-resume path (the if-branch sets
	//     payloadRef + typedOutputs and falls through).
	if err := appendNodeCompleted(ictx.log, path, NodeCompletedData{
		Outcome:    string(OutcomeOK),
		OutputsRef: payloadRef,
	}); err != nil {
		return "", fmt.Errorf("engine.runSignalStep: %w", err)
	}

	// 7c. Update RunState.Completed mirror.
	ictx.runstate.RecordCompleted(path, NodeResult{
		Outcome:    OutcomeOK,
		Outputs:    typedOutputs,
		OutputsRef: payloadRef,
	})
	return OutcomeOK, nil
}

// buildWherePredicateWithScope compiles a signal step's where: clause into a
// payload matcher (F18). The clause is ONE `{{ }}` envelope, parsed ONCE via
// template.ParseExpr — the SAME bounded-boolean grammar if:/until: use (no
// arithmetic/calls/loops) — and NEVER string-substituted: unlike the old
// substitute-then-parse builder, no ref's value is ever rendered into the
// where: string before parsing, so a value containing predicate metacharacters
// (quotes, parens, boolean operators) cannot alter the expression's structure.
//
// Two synchronous, PERMANENT failure modes are surfaced here, before the broker
// is ever polled (both are author bugs — re-running is identical):
//
//  1. A grammar parse error (an AWF1036 the validator should have caught).
//  2. An unresolvable OUTER ref. Every non-`signal` ref (input.*, step.*, run.*,
//     <as>.*, …) is loop-INVARIANT — identical for every candidate payload — so
//     each is resolved ONCE up front against engineScope. If any fails to
//     resolve (a typo'd/undeclared ref the static validator can't catch, since
//     where: outer refs aren't walked by ir.walkRefs), fail fast+permanent —
//     restoring the old eager-Substitute behavior. This is a resolvability
//     CHECK only: the value is NOT spliced back into the predicate string (that
//     was the injection vector); the expr stays parsed-once. Without this
//     pre-check the error would hide inside the per-candidate MatchFunc, where
//     the broker's skip-on-error contract turns it into an infinite block (no
//     timeout) or an eventual misleading timeout.
//
// signal.* refs are deliberately NOT pre-checked: they legitimately vary per
// payload, and a missing payload field is a real per-candidate non-match (keep
// waiting), not an author bug.
//
// The returned MatchFunc re-evaluates the SAME parsed expr against a FRESH
// signalScope for each candidate payload: `signal.*` resolves against that
// candidate's payloadScope, every other root against engineScope. A candidate
// whose payload is not a JSON object → (false, err) so tryConsumeMatching SKIPS
// it (leaves it buffered) rather than consuming it.
func buildWherePredicateWithScope(where string, engineScope template.Scope) (signal.MatchFunc, error) {
	expr, err := template.ParseExpr(template.UnwrapEnvelope(where))
	if err != nil {
		return nil, fmt.Errorf("parse where expression %q: %w", where, err)
	}
	// Eager outer-ref resolvability pre-check (fail-fast restore — see doc above).
	for _, ref := range template.References(expr) {
		ref := ref
		if isSignalRootRef(&ref) {
			continue // signal.* varies per payload — deferred to the MatchFunc.
		}
		if _, rerr := engineScope.Resolve(&ref); rerr != nil {
			return nil, fmt.Errorf("resolve outer ref: %w", rerr)
		}
	}
	return func(payload []byte) (bool, error) {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber() // keep numbers as json.Number — the evaluator's toFloat handles it
		if derr := dec.Decode(&obj); derr != nil {
			return false, fmt.Errorf("where: signal payload is not a JSON object: %w", derr)
		}
		scope := &signalScope{payload: newPayloadScope(obj), outer: engineScope}
		return template.EvalBool(expr, scope)
	}, nil
}
