package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/live"
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
//     Rejected               → ExitRunFailed, gate-rejection cause on stderr.
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
	resolver agent.Resolver,
	ld *ir.LoadedDefinition,
	rs *engine.RunState,
	handles map[string]container.Handle,
	log state.Log,
	blobs state.Blobs,
	stdout, stderr io.Writer,
	runID, opName, successSuffix string,
	resume bool,
	assets map[string]engine.RunStartedAsset,
	inputFiles map[string]string,
	broker *signal.Broker,
	liveRoot live.Root,
	skipTeardown *bool,
	rerunFrom string,
) int {
	tap := r.agentEventTap(stderr)
	dispatcher := &engine.LocalDispatcher{
		Backend:          backend,
		Handles:          handles,
		ComposeFiles:     ld.ComposeFiles,
		Resolver:         resolverOrEmpty(resolver),
		AgentEventTap:    tap,
		RenderAgentEvent: newAgentEventRenderer(tap),
		StepCostLine:     true,
		RunState:         rs,
		Blobs:            blobs,
	}
	outcome, runErr := engine.Run(ctx, ld, rs, dispatcher, log, blobs, clock.System{}, engine.RunOptions{
		Tap:           stdout,
		Broker:        broker,
		Assets:        assets,
		InputFiles:    inputFiles,
		LiveFinalizer: liveDispatchFinalizer(liveRoot),
		Resume:        resume,
		RerunFrom:     rerunFrom,
	})

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
		printRunCostSummary(stdout, log)
	}

	switch outcome {
	case engine.OutcomeOK:
		fprintf(stdout, "run %s: ok%s\n", runID, successSuffix)
		return ExitOK
	case engine.OutcomeRetryableFailure, engine.OutcomePermanentFailure:
		fprintf(stderr, "run %s: %s: %v\n", runID, outcome, runErr)
		return ExitRunFailed
	case engine.OutcomeRejected:
		// A gate exhausted max_attempts without passing — a legitimate terminal
		// outcome, not an internal error. run.finished{rejected} is the durable
		// distinguisher; surface it as a run failure (ExitRunFailed), never the
		// ExitUsage "internal error" path (which is reserved for an empty,
		// interpreter-bug outcome).
		fprintf(stderr, "run %s: rejected: %v\n", runID, runErr)
		return ExitRunFailed
	default:
		fprintf(stderr, "run %s: internal error: %v\n", runID, runErr)
		return ExitUsage
	}
}

// runMetrics is a per-run cost/token rollup folded from node.completed events.
// HasCost is true iff at least one agent step carried a reported cost source,
// which is distinct from TotalUSD==0: an unpriced adapter (no pricing package)
// should read as "cost unknown", never a misleading "$0".
// HasSplit is true iff at least one derived step contributed an input/output
// dollar split; InUSD/OutUSD accumulate that split across all derived steps.
// ReportedUSD accumulates reported-source step costs and is shown in the
// parenthetical split only when HasSplit is also true.
type runMetrics struct {
	TotalUSD    float64
	HasCost     bool
	InUSD       float64
	OutUSD      float64
	ReportedUSD float64
	HasSplit    bool
	InTok       int
	OutTok      int
	Turns       int
	AgentSteps  int
}

// foldRunMetrics sums per-step agent metrics across a run's folded events
// (matching obs's per-step sum; cross-call dedup is obs's rollup concern, not
// here). Shared by printRunCostSummary and `awf ls`.
func foldRunMetrics(events []state.Event) runMetrics {
	var m runMetrics
	for _, e := range events {
		if e.Type != engine.EventNodeCompleted {
			continue
		}
		var d engine.NodeCompletedData
		if err := json.Unmarshal(e.Data, &d); err != nil || d.Metrics == nil {
			continue
		}
		m.AgentSteps++
		m.TotalUSD += d.Metrics.Cost.Total
		if d.Metrics.Cost.Source != "" {
			m.HasCost = true
		}
		switch d.Metrics.Cost.Source {
		case agent.CostSourceDerived:
			m.InUSD += d.Metrics.Cost.Input
			m.OutUSD += d.Metrics.Cost.Output
			m.HasSplit = true
		case agent.CostSourceReported:
			m.ReportedUSD += d.Metrics.Cost.Total
		}
		m.InTok += d.Metrics.Tokens.Input
		m.OutTok += d.Metrics.Tokens.Output
		m.Turns += d.Metrics.Turns
	}
	return m
}

// printRunCostSummary folds the just-written log and prints a one-line cost/
// token rollup — but ONLY when at least one node.completed carried agent
// metrics, so code-step-only runs print nothing. Sibling of the Phase-5 live
// tap: never routes through obs (keeps obs off the hot path). Best-effort — a
// fold error just suppresses the summary.
func printRunCostSummary(stdout io.Writer, log state.Log) {
	events, err := log.Fold()
	if err != nil {
		return
	}
	m := foldRunMetrics(events)
	if m.AgentSteps == 0 {
		return
	}
	costStr := formatUSD(m.TotalUSD)
	if m.HasSplit {
		split := "in " + formatUSD(m.InUSD) + " / out " + formatUSD(m.OutUSD)
		if m.ReportedUSD > 0 {
			split += " + reported " + formatUSD(m.ReportedUSD)
		}
		costStr = formatUSD(m.TotalUSD) + " (" + split + ")"
	}
	fprintf(stdout, "  cost: %s · %d tok (%d in / %d out) · %d turns across %d agent step(s)\n",
		costStr, m.InTok+m.OutTok, m.InTok, m.OutTok, m.Turns, m.AgentSteps)
}
