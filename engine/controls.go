package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// pollControls runs in a goroutine spawned by engine.Run. It polls
// broker.CheckPauseCancel every broker.PollInterval() interval; on detection,
// it sets in-memory flags on runstate (SetCancelled+SetCancelReason OR
// SetPaused) and calls cancel() to propagate ctx-cancel through running
// goroutines. It does NOT write to the log — the interpreter (engine.Run,
// AFTER the recursive walk returns) appends run.paused / run.cancelled
// events itself. This preserves the CLAUDE.md "interpreter is the only
// writer to state" invariant.
//
// Cancel-wins resolution: if BOTH pause.json and cancel.json are present,
// cancel takes precedence (terminal beats non-terminal). Operators who
// pause-then-cancel get the cancel.
//
// Exit conditions:
//   - ctx.Done() fires (engine.Run finished and called the deferred cancel).
//   - A pause or cancel was detected → flags set, ctx cancelled, return.
//
// The done channel is closed on exit (regardless of which exit) so
// engine.Run can synchronize cleanup before reading the flags.
//
// On broker I/O error during polling: log NOTHING (no logger plumbed),
// continue the loop. A permanent broker error would mean the control
// directory is unreadable; the engine continues without cancel/pause
// detection. Phase 3 minimum — Phase 6 obs will surface poll-errors.
func pollControls(
	ctx context.Context,
	broker *signal.Broker,
	runstate *RunState,
	cancel context.CancelFunc,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(broker.PollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pauseReq, cancelReq, err := broker.CheckPauseCancel()
			if err != nil {
				continue // I/O error; try again next tick
			}
			if cancelReq != nil {
				runstate.SetCancelled(true)
				runstate.SetCancelReason(cancelReq.Reason)
				cancel()
				return
			}
			if pauseReq != nil {
				runstate.SetPaused(&PauseMarker{
					NodePath: pauseReq.NodePath,
					Reason:   pauseReq.Reason,
				})
				cancel()
				return
			}
		}
	}
}

// appendTerminalControlEvents inspects runstate's pause/cancel flags after
// the recursive walk returns. If either is set, appends the corresponding
// terminal event + returns the matching sentinel error (signal.ErrCancelled
// or signal.ErrPaused). If neither is set, returns nil (engine.Run returns
// the natural (oc, err) from interpNodes).
//
// Cancel takes precedence over pause (terminal beats non-terminal — same
// resolution as pollControls).
//
// Extracted from engine.Run for unit-testability (H1 fix): the
// flag-state-to-event-emission mapping is the regression-prone part. The
// goroutine lifecycle (cancel + <-pollDone synchronization) is engine.Run-
// specific and remains inlined there; it's tested separately by
// TestPollControls* (this file).
func appendTerminalControlEvents(log state.Log, rs *RunState) error {
	if rs.IsCancelled() {
		if err := appendRunCancelled(log, rs.LookupCancelReason()); err != nil {
			return err
		}
		return signal.ErrCancelled
	}
	if pm := rs.LookupPaused(); pm != nil {
		if err := appendRunPaused(log, pm.NodePath, pm.Reason); err != nil {
			return err
		}
		return signal.ErrPaused
	}
	return nil
}

// appendRunPaused appends a run.paused event + fsyncs the log. fsync is
// required because the event is the breakpoint marker; losing it to a torn
// tail would mean a paused run looks like an in-flight one to the operator.
func appendRunPaused(log state.Log, nodePath, reason string) error {
	return appendSyncedEvent(log, EventRunPaused, "run.paused",
		RunPausedData{NodePath: nodePath, Reason: reason})
}

// appendRunCancelled appends a run.cancelled event + fsyncs. TERMINAL —
// must be durable before engine.Run returns so `awf resume`'s refusal check
// can see it.
func appendRunCancelled(log state.Log, reason string) error {
	return appendSyncedEvent(log, EventRunCancelled, "run.cancelled",
		RunCancelledData{Reason: reason})
}

// appendSyncedEvent serializes data as JSON, appends a state.Event of the
// given type, and fsyncs the log. Used by appendRunPaused/appendRunCancelled
// (slice 3.5) for run-level terminal markers that must survive a torn-tail
// crash. The label appears in error messages to identify the failing site.
func appendSyncedEvent(log state.Log, eventType, label string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("engine: marshal %s: %w", label, err)
	}
	if err := log.Append(state.Event{Type: eventType, Data: raw}); err != nil {
		return fmt.Errorf("engine: append %s: %w", label, err)
	}
	if err := log.Sync(); err != nil {
		return fmt.Errorf("engine: sync after %s: %w", label, err)
	}
	return nil
}
