package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// RunWithRetry runs dispatcher.Run up to policy.Attempts times. Between
// attempts it sleeps policy.BackoffFor(attempt) via clk.Sleep (deterministic
// under clock.Fake or testing/synctest with clock.System). Each non-final
// attempt's outcome is recorded as a retry.attempt event in log; the final
// attempt's outcome is NOT recorded by RunWithRetry — the caller (slice 2.5's
// interpreter) is responsible for the final node.completed (success) or
// node.failed (exhausted / permanent) event.
//
// Returns the LAST attempt's DispatchResult + IOChunk channel. Intermediate
// IOChunk channels are drained (so the per-attempt streams don't leak); only
// the final attempt's channel is forwarded to the live tap.
//
// Termination conditions, in order:
//   - Outcome == OutcomeOK              → return (dr, chunks, nil)
//   - Outcome == OutcomePermanentFailure→ return (dr, chunks, dr.Err)
//   - dispatcher.Run returns a non-nil error (interpreter bug — e.g. unknown
//     container, ErrUnsupportedKind) → return (dr, nil, err)
//   - attempt == policy.Attempts (last)  → return (dr, chunks, lastErr)
//   - clk.Sleep returns ctx.Err()        → return (last dr, nil, ctx.Err())
//
// Revision #12: drain happens AFTER dispatcher.Run returns and BEFORE the
// next sleep — preventing Phase 4 streaming-backend deadlock where the
// channel buffer fills during sleep with the writer goroutine still pushing.
//
// retry.attempt event emission: best-effort. If Log.Append fails for the
// retry.attempt event, RunWithRetry surfaces the error AS the final returned
// error (we can't proceed without trusting the log).
func RunWithRetry(
	ctx context.Context,
	dispatcher Dispatcher,
	intent NodeIntent,
	policy retry.Policy,
	clk clock.Clock,
	log state.Log,
) (DispatchResult, <-chan container.IOChunk, error) {
	if policy.Attempts < 1 {
		return DispatchResult{}, nil, fmt.Errorf("engine.RunWithRetry: policy.Attempts = %d, want ≥ 1", policy.Attempts)
	}

	var dr DispatchResult
	var chunks <-chan container.IOChunk
	var lastErr error

	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		// Preceding-sleep for attempts > 1 (BackoffFor(1) = 0; spec §6 backoff).
		if attempt > 1 {
			if err := clk.Sleep(ctx, policy.BackoffFor(attempt)); err != nil {
				return dr, nil, err
			}
		}

		dr, chunks, lastErr = dispatcher.Run(ctx, intent)
		// Interpreter-bug class (unknown container, unsupported kind): halt
		// immediately, surface as-is. NOT a retryable failure.
		if lastErr != nil {
			return dr, nil, lastErr
		}

		switch dr.Outcome {
		case OutcomeOK:
			return dr, chunks, nil
		case OutcomePermanentFailure:
			return dr, chunks, dr.Err
		case OutcomeRetryableFailure:
			// fall through to attempt accounting + retry/exhaustion check
		default:
			return dr, chunks, fmt.Errorf("engine.RunWithRetry: dispatcher returned unknown outcome %q at path %q", dr.Outcome, intent.Path)
		}

		// At this point dr.Outcome == retryable_failure. If this was the last
		// attempt, halt with the attempt's error — return chunks intact so the
		// caller (slice 2.5's live tap) can drain them.
		lastErr = dr.Err
		if attempt == policy.Attempts {
			return dr, chunks, lastErr
		}

		// Revision #12: drain THIS attempt's chunks NOW, BEFORE the next sleep.
		// If we slept first with chunks still in-flight (Phase 4 streaming
		// Docker backend), the channel buffer could fill and the backend's
		// writer goroutine would block — eventually deadlocking. For the Phase
		// 2 fake the channel is pre-closed so this is a no-op, but the order
		// matters for Phase 4.
		if chunks != nil {
			for range chunks {
				// discard
			}
		}
		if intent.agentEventSink != nil {
			if err := intent.agentEventSink.flushWait(); err != nil {
				return DispatchResult{}, nil, fmt.Errorf("engine.RunWithRetry: flush live agent.event at path %q: %w", intent.Path, err)
			}
		}

		// Emit retry.attempt event AFTER draining (chunk drain is local; event
		// emission touches the log, the only other failure point in this loop).
		errStr := ""
		if dr.Err != nil {
			errStr = dr.Err.Error()
		}
		data, err := json.Marshal(RetryAttemptData{
			N:       attempt,
			Outcome: string(dr.Outcome),
			Error:   errStr,
		})
		if err != nil {
			return dr, nil, fmt.Errorf("engine.RunWithRetry: marshal retry.attempt at path %q: %w", intent.Path, err)
		}
		if err := log.Append(state.Event{
			Type: EventRetryAttempt,
			Path: intent.Path,
			Data: data,
		}); err != nil {
			return dr, nil, fmt.Errorf("engine.RunWithRetry: append retry.attempt at path %q: %w", intent.Path, err)
		}
		// Note: retry.attempt rides the next Log.Sync (the eventual
		// node.completed / node.failed) per the durability class decision in
		// the slice 2.4 plan Design question 3.
	}

	// Unreachable — the loop above always returns inside the body.
	return dr, chunks, lastErr
}
