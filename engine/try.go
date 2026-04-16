package engine

import (
	"context"
	"errors"
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// runTry is the Try handler (Phase 3 spec §5.3 + design §B). Implements the
// do → catch → finally state machine with the rules pinned in design decision
// 7 (unconditional catch) and this slice's design question 3 (ctx-cancel
// re-check after Finally).
//
// State machine:
//
//  1. Run Do (recursive walk over n.Do).
//  2. Inspect Do's result:
//     a. *SkipUnwind escaped Do → try is a PASSTHROUGH per spec §5.6. Try is
//     NOT a target scope for skip. Skip Catch. Append node.skipped{path=tryPath}
//     for trace (§5.3). Run Finally. Then RE-RAISE the SkipUnwind to the
//     caller so it propagates to the next enclosing loop/gate/parallel/run.
//     b. err != nil (any other) → run Catch if present; Catch's err, if
//     any, becomes the propagating error; if no Catch, Do's err propagates.
//     c. err == nil, oc == ok → skip Catch.
//  3. ALWAYS run Finally (even on ctx-cancel, even if Catch errored).
//  4. If Finally errored, Finally's error supersedes.
//  5. Final check (design question 3): if ctx is cancelled, return
//     (OutcomeRetryableFailure, ctx.Err()) even if Do/Catch/Finally all
//     returned ok — the cancellation signal must reach the parent (matters
//     for slice 3.2's parallel handler).
//
// Phase 3 design decision 7: unconditional catch. Catch absorbs ANY non-skip
// non-nil error from Do. Typed-kind matching is a spec §5.3 follow-up.
func runTry(
	ctx context.Context,
	n *ir.Try,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	// 1. Run Do.
	doOC, doErr := interpNodes(ctx, n.Do, path+".do", wf, runstate, dispatcher, log, blobs, clk, tap)

	// 2a. SkipUnwind escaped Do → terminal-ok. Skip Catch.
	var su *SkipUnwind
	skipped := errors.As(doErr, &su)

	// 2b. Other error from Do → run Catch.
	propagatedErr := doErr
	propagatedOC := doOC
	if doErr != nil && !skipped && len(n.Catch) > 0 {
		// Catch absorbs the error (unconditional catch). Catch may itself fail,
		// in which case Catch's error becomes the propagated error.
		catchOC, catchErr := interpNodes(ctx, n.Catch, path+".catch", wf, runstate, dispatcher, log, blobs, clk, tap)
		propagatedOC = catchOC
		propagatedErr = catchErr
	}

	// 2a continued: if Do skipped, runTry is a PASSTHROUGH — spec §5.6 lists
	// loop/gate/parallel/run as the skip target scopes; try is NOT listed.
	// Append node.skipped{path=tryPath} for trace (§5.3). Then re-raise
	// the SkipUnwind so the next enclosing scope (Run, runLoop, future
	// parallel/gate) catches it.
	// If appendNodeSkipped fails, track in pendingErr so that Finally still
	// runs ("ALWAYS run Finally") before we propagate the error.
	var pendingErr error
	if skipped {
		// Spec §5.6: skip terminates the NEAREST enclosing loop/gate/parallel/run.
		// Try is NOT a target scope — skip propagates THROUGH a try (running
		// Finally on the way per spec §5.3). Re-raise the SkipUnwind after Finally
		// runs so the next enclosing scope catches it.
		if appendErr := appendNodeSkipped(log, path, su.Reason); appendErr != nil {
			pendingErr = appendErr
		}
		// propagatedErr stays as the SkipUnwind from doErr — Finally runs, then
		// (after Finally clean) we re-raise it to the caller.
		propagatedOC = OutcomeOK // SkipUnwind carries the unwind signal; outcome is OK
	}

	// 3. ALWAYS run Finally (even on ctx-cancel, even if Catch errored).
	if len(n.Finally) > 0 {
		finallyOC, finallyErr := interpNodes(ctx, n.Finally, path+".finally", wf, runstate, dispatcher, log, blobs, clk, tap)
		if finallyErr != nil {
			// 4. Finally errored — its error wins.
			return finallyOC, finallyErr
		}
	}

	// If appendNodeSkipped errored AND Finally was clean, propagate the append error.
	if pendingErr != nil {
		return "", pendingErr
	}

	// 5. Final ctx check (design question 3). Cancellation signal supersedes
	// Do/Catch errors — the parent NEEDS the cancellation signal (slice 3.2's
	// parallel handler uses errors.Is(err, context.Canceled) to distinguish
	// sibling-cancelled branches from independently-failed ones).
	// Note: Finally errors already returned above; this check is intentionally
	// unconditional (no propagatedErr == nil guard).
	if ctxErr := ctx.Err(); ctxErr != nil {
		return OutcomeRetryableFailure, ctxErr
	}

	return propagatedOC, propagatedErr
}
