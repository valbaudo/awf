package cli

import (
	"fmt"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// resumeAdmission decides whether a folded run's event log permits (re-)entry by
// awf resume. It governs ONLY the terminal-outcome guard; pin checks (definition
// digest, runtime drift) are enforced separately by the caller and are NOT
// relaxed by force.
//
// Without force: any terminal marker (run.finished of any outcome, run.cancelled,
// node.failed) refuses; an interrupted run (no terminal marker) is admitted —
// this preserves the historical guard exactly.
//
// With force: a run whose terminal rollup is permanent_failure / rejected /
// cancelled is admitted (run.finished.Outcome is the authority when present;
// run.cancelled and the crash-window node.failed outcome otherwise). A finished
// ok run is never admitted. retryable_failure is also admitted under force (a
// harmless superset). label is the terminal outcome string for the caller's
// warning ("" for an interrupted run). The LAST run.finished wins (a force-
// resumed run appends a second run.finished), so this does NOT break on the
// first match.
//
// COORDINATION: the resume-retryable-failure scope-b effort relaxes the
// force=false path to also admit retryable_failure (its §5.1 guard). At merge,
// reconcile to one helper: admit retryable_failure (force-independent) ∪
// {permanent_failure, rejected, cancelled} (force-only).
func resumeAdmission(runID string, events []state.Event, force bool) (admit bool, refuseMsg string, label string) {
	var finished *engine.RunFinishedData
	cancelled := false
	for _, e := range events {
		switch e.Type {
		case engine.EventRunFinished:
			d, err := engine.RunFinishedDataFromEvent(e)
			if err != nil {
				return false, fmt.Sprintf("awf resume: run %q has a corrupt run.finished event: %v\n", runID, err), ""
			}
			finished = &d
		case engine.EventRunCancelled:
			cancelled = true
		}
	}

	if !force {
		if finished != nil {
			switch engine.Outcome(finished.Outcome) {
			case engine.OutcomeOK:
				return false, fmt.Sprintf("awf resume: run %q already finished (ok). Cannot resume a completed run.\n", runID), ""
			default:
				return false, fmt.Sprintf("awf resume: run %q finished with outcome %q. Re-run with --force to re-enter after fixing the cause.\n", runID, finished.Outcome), ""
			}
		}
		if cancelled {
			return false, fmt.Sprintf("awf resume: run %q was cancelled (run.cancelled in log). Cannot resume a cancelled run; start a new run id.\n", runID), ""
		}
		for _, e := range events {
			if e.Type == engine.EventNodeFailed {
				return false, fmt.Sprintf("awf resume: run %q terminated on a failed step (node.failed at path %q in log). Re-run with --force to re-enter after fixing the cause.\n", runID, e.Path), ""
			}
		}
		return true, "", ""
	}

	// force: admit a non-ok terminal run.
	if finished != nil {
		switch engine.Outcome(finished.Outcome) {
		case engine.OutcomeOK:
			return false, fmt.Sprintf("awf resume: run %q already finished (ok). Nothing to resume, even with --force.\n", runID), ""
		case engine.OutcomePermanentFailure, engine.OutcomeRejected, engine.OutcomeRetryableFailure:
			return true, "", finished.Outcome
		default:
			return false, fmt.Sprintf("awf resume: run %q has an unrecognized terminal outcome %q; not resumable even with --force.\n", runID, finished.Outcome), ""
		}
	}
	if cancelled {
		return true, "", "cancelled"
	}
	for _, e := range events {
		if e.Type == engine.EventNodeFailed {
			d, err := engine.NodeFailedDataFromEvent(e)
			if err != nil {
				return false, fmt.Sprintf("awf resume: run %q has a corrupt node.failed event: %v\n", runID, err), ""
			}
			// node.failed only ever carries retryable_failure / permanent_failure
			// (failStep; see NodeFailedData doc). A gate's "rejected" rolls up via
			// run.finished, never a node.failed event — so it is NOT checked here.
			switch engine.Outcome(d.Outcome) {
			case engine.OutcomePermanentFailure, engine.OutcomeRetryableFailure:
				return true, "", d.Outcome
			default:
				return false, fmt.Sprintf("awf resume: run %q terminated with an unrecognized failure %q; not resumable even with --force.\n", runID, d.Outcome), ""
			}
		}
	}
	return true, "", "" // interrupted run (no terminal marker)
}
