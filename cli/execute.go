package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// agentEventTap returns the io.Writer the dispatcher writes agent-event
// lines to. If r.AgentEventTap is explicitly set (test injection), that
// wins; otherwise defaults to stderr — the production path. Returning
// io.Discard would suppress the live tap entirely; we don't default to
// that because operators need to see what their agents are doing.
func (r *Runner) agentEventTap(stderr io.Writer) io.Writer {
	if r.AgentEventTap != nil {
		return r.AgentEventTap
	}
	return stderr
}

// runAndFinish is the shared tail of `awf run` and `awf resume`. Both
// subcommands diverge in their setup — run.started vs run.resumed framing,
// log-create vs log-open, fresh RunState vs folded RunState, Create-handles
// position — but converge on the same closing sequence:
//
//  1. Build the LocalDispatcher and call engine.Run with the prepared
//     RunState. The interpreter writes all per-node events (node.completed,
//     branch.taken, loop.iter) — the CLI never does.
//  2. On a terminal outcome (non-empty), append run.finished + Sync. The
//     CLI is the only writer of run.* framing events.
//  3. Map outcome → exit code:
//     OutcomeOK              → ExitOK,        success line on stdout.
//     Retryable/Permanent    → ExitRunFailed, cause on stderr.
//     "" (interpreter bug)   → ExitUsage,     "internal error" on stderr.
//
// opName is the verb-prefix for error messages ("awf run" or "awf resume").
// successSuffix is appended to the success line — "" for `awf run`,
// " (resumed)" for `awf resume`.
//
// Slice 4.5: backend is passed in (was read from r.Backend). The caller
// holds it as a local variable so sequential runner.Run(...) calls don't
// leak a constructed Backend across invocations.
//
// Handles creation lives in the caller, not here — `awf run` Creates BEFORE
// OpenLogExclusive (so a Create-fail leaves no orphan log), `awf resume`
// Creates AFTER refusal checks. The ordering difference is load-bearing;
// extracting Create into the helper would change `awf run`'s slice-2.5
// behavior.
func (r *Runner) runAndFinish(
	ctx context.Context,
	backend container.Backend,
	ld *ir.LoadedDefinition,
	rs *engine.RunState,
	handles map[string]container.Handle,
	log state.Log,
	blobs state.Blobs,
	stdout, stderr io.Writer,
	runID, opName, successSuffix string,
	broker *signal.Broker,
	skipTeardown *bool,
) int {
	dispatcher := &engine.LocalDispatcher{
		Backend:       backend,
		Handles:       handles,
		ComposeFiles:  ld.ComposeFiles,
		Resolver:      r.resolverOrEmpty(),
		AgentEventTap: r.agentEventTap(stderr),
	}
	outcome, runErr := engine.Run(ctx, ld, rs, dispatcher, log, blobs, clock.System{}, stdout, broker)

	// Phase 3 slice 3.5: ErrPaused is a non-terminal halt. No run.finished
	// event is written; containers stay up; resume continues in a new epoch.
	if errors.Is(runErr, signal.ErrPaused) {
		*skipTeardown = true
		fprintf(stdout, "run %s: paused — use `awf resume %s <workflow>` to continue\n", runID, runID)
		return ExitOK
	}
	// ErrCancelled: the engine has ALREADY appended terminal run.cancelled.
	// Containers DO tear down (the deferred Destroy fires normally).
	if errors.Is(runErr, signal.ErrCancelled) {
		fprintf(stdout, "run %s: cancelled\n", runID)
		return ExitOK
	}

	if outcome != "" {
		finishedData, mErr := json.Marshal(engine.RunFinishedData{Outcome: string(outcome)})
		if mErr != nil {
			fprintf(stderr, "%s: marshal run.finished: %v\n", opName, mErr)
			return ExitUsage
		}
		if err := log.Append(state.Event{Type: engine.EventRunFinished, Data: finishedData}); err != nil {
			fprintf(stderr, "%s: append run.finished: %v\n", opName, err)
			return ExitUsage
		}
		if err := log.Sync(); err != nil {
			fprintf(stderr, "%s: sync log after run.finished: %v\n", opName, err)
			return ExitUsage
		}
	}

	switch outcome {
	case engine.OutcomeOK:
		fprintf(stdout, "run %s: ok%s\n", runID, successSuffix)
		return ExitOK
	case engine.OutcomeRetryableFailure, engine.OutcomePermanentFailure:
		fprintf(stderr, "run %s: %s: %v\n", runID, outcome, runErr)
		return ExitRunFailed
	default:
		fprintf(stderr, "run %s: internal error: %v\n", runID, runErr)
		return ExitUsage
	}
}
