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
	// 1. Resume-check.
	if _, done := runstate.LookupCompleted(path); done {
		return OutcomeOK, nil
	}

	// 2. Nil-broker defense.
	if broker == nil {
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
	if entry, ok := runstate.LookupSignalReceivedAt(path); ok {
		payloadRef = entry.PayloadRef
		// Re-derive typed payload from CAS if schema declared.
		if ss.OutputSchema != nil && payloadRef != "" {
			raw, gerr := blobs.Get(payloadRef)
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
		appendNodeStarted(log, path, "signal")

		// 4. Block on broker.Receive.
		timeout := time.Duration(0)
		if ss.Timeout != nil {
			timeout = time.Duration(*ss.Timeout)
		}
		// Keyed consumption (spec §3.5 C5). With ss.Where, build a payload
		// predicate: substitute {{ … }} slots against the ENGINE scope ONCE
		// (the correlation value, e.g. {{ hyp.id }}, is constant across
		// candidates), parse the rendered string as a bounded boolean Expr,
		// then match each candidate payload via a payloadScope. The earliest
		// matching signal is consumed; non-matching signals stay buffered.
		var (
			d    signal.Delivery
			rerr error
		)
		if ss.Where != "" {
			match, perr := buildWherePredicate(ss.Where, runstate, wf, path)
			if perr != nil {
				// Slot substitution / expr parse failed at runtime — an author
				// bug the validator should have caught, but a {{ }} ref that
				// resolves to a composite (AWF4004) or an unresolved engine ref
				// (AWF4002) only surfaces here. Permanent: re-running is identical.
				return failStep(log, path, OutcomePermanentFailure,
					fmt.Errorf("signal %q where: %w", ss.Await, perr))
			}
			d, rerr = broker.ReceiveMatching(ctx, ss.Await, timeout, match)
		} else {
			d, rerr = broker.Receive(ctx, ss.Await, timeout)
		}
		if rerr != nil {
			if errors.Is(rerr, context.DeadlineExceeded) {
				return failStep(log, path, OutcomeRetryableFailure,
					fmt.Errorf("signal %q await timeout (%s)", ss.Await, timeout))
			}
			// M7 fix: ctx.Canceled caused by the operator (cancel.json /
			// pause.json detected by pollControls) is NOT a step failure.
			// engine.Run will append the terminal run.cancelled / run.paused
			// event after this handler returns. Don't emit node.failed —
			// that'd create a redundant terminal record + block resume on
			// cli/resume.go's node.failed refusal class.
			if runstate.IsCancelled() || runstate.LookupPaused() != nil {
				return "", rerr
			}
			// True transport / Ctrl-C error — emit node.failed.
			return failStep(log, path, OutcomeRetryableFailure,
				fmt.Errorf("signal %q await: %w", ss.Await, rerr))
		}

		// 5. Validate payload against output_schema if declared. Empty payload
		//    + schema declared → ValidateAgainstSchema rejects empty input
		//    naturally (engine/schema.go:31-33). No separate len(payload) > 0
		//    guard (C5 fix).
		if ss.OutputSchema != nil {
			validated, verr := ValidateAgainstSchema(d.Payload, ss.OutputSchema)
			if verr != nil {
				return failStep(log, path, OutcomeRetryableFailure,
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
			ref, perr := blobs.Put(canonicalBytes)
			if perr != nil {
				return "", fmt.Errorf("engine.runSignalStep: put payload at %q: %w", path, perr)
			}
			payloadRef = ref
		} else if len(d.Payload) > 0 {
			// No schema declared → CAS the raw bytes as-is (no canonicalization
			// target; the operator's bytes are the wire form).
			ref, perr := blobs.Put(d.Payload)
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
		if err := log.Append(state.Event{Type: EventSignalReceived, Path: path, Data: sigData}); err != nil {
			return "", fmt.Errorf("engine.runSignalStep: append signal.received at %q: %w", path, err)
		}
		if err := log.Sync(); err != nil {
			return "", fmt.Errorf("engine.runSignalStep: sync after signal.received at %q: %w", path, err)
		}
		// Mirror in RunState (used by Phase 6 obs + as the path-keyed lookup
		// for any future re-entry of the same path).
		runstate.AppendSignal(ss.Await, SignalEntry{Seq: seq, PayloadRef: payloadRef})
		runstate.RecordSignalReceivedAt(path, SignalReceivedEntry{
			Seq:        seq,
			PayloadRef: payloadRef,
		})
	}

	// 7b. Append node.completed + fsync. Reached by both the fresh-receive
	//     path (above) AND the half-commit-resume path (the if-branch sets
	//     payloadRef + typedOutputs and falls through).
	completedData, mErr := json.Marshal(NodeCompletedData{
		Outcome:    string(OutcomeOK),
		OutputsRef: payloadRef,
	})
	if mErr != nil {
		return "", fmt.Errorf("engine.runSignalStep: marshal node.completed at %q: %w", path, mErr)
	}
	if err := log.Append(state.Event{Type: EventNodeCompleted, Path: path, Data: completedData}); err != nil {
		return "", fmt.Errorf("engine.runSignalStep: append node.completed at %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return "", fmt.Errorf("engine.runSignalStep: sync after node.completed at %q: %w", path, err)
	}

	// 7c. Update RunState.Completed mirror.
	runstate.RecordCompleted(path, NodeResult{
		Outcome:    OutcomeOK,
		Outputs:    typedOutputs,
		OutputsRef: payloadRef,
	})
	return OutcomeOK, nil
}

// buildWherePredicate compiles a signal step's where: clause into a payload
// matcher. The {{ … }} slots substitute ONCE against the engine scope (the
// correlation value is constant across candidate signals); the rendered string
// parses as a bounded boolean Expr (reusing the SAME evaluator as if/loop/gate —
// no arithmetic). The returned MatchFunc parses each candidate payload as JSON
// and evaluates the expr against a payloadScope. A candidate whose payload is
// not a JSON object → (false, err) so tryConsumeMatching SKIPS it (leaves it
// buffered) rather than consuming it.
func buildWherePredicate(where string, runstate *RunState, wf *ir.Workflow, path string) (signal.MatchFunc, error) {
	engineScope := NewScope(runstate, wf, path)
	rendered, err := template.Substitute(where, engineScope)
	if err != nil {
		return nil, fmt.Errorf("substitute where slots: %w", err)
	}
	expr, err := template.ParseExpr(template.UnwrapEnvelope(rendered))
	if err != nil {
		return nil, fmt.Errorf("parse where expression %q: %w", rendered, err)
	}
	return func(payload []byte) (bool, error) {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber() // keep numbers as json.Number — the evaluator's toFloat handles it
		if derr := dec.Decode(&obj); derr != nil {
			return false, fmt.Errorf("where: signal payload is not a JSON object: %w", derr)
		}
		return template.EvalBool(expr, newPayloadScope(obj))
	}, nil
}
