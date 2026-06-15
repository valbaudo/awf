package cli

import (
	"fmt"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// resumeAdmission decides whether a folded run's log permits (re-)entry by awf
// resume. The ONLY refusal is a finished-ok run (a genuine no-op). Every non-ok
// terminal outcome (permanent_failure / rejected / retryable_failure / cancelled)
// and every interrupted run is admitted. Pins are enforced separately by the
// caller (gated by `if *from == ""`) and are NOT relaxed here. label is the
// terminal outcome for the caller's non-fatal note; "" = resume silently (an
// interrupted run, or a crash-window node.failed with no run.finished).
func resumeAdmission(runID string, events []state.Event) (admit bool, refuseMsg, label string) {
	var finished *engine.RunFinishedData
	cancelled := false
	for _, e := range events {
		switch e.Type {
		case engine.EventRunFinished:
			d, err := engine.RunFinishedDataFromEvent(e)
			if err != nil {
				return false, fmt.Sprintf("awf resume: run %q has a corrupt run.finished event: %v\n", runID, err), ""
			}
			finished = &d // latest run.finished wins
		case engine.EventRunCancelled:
			cancelled = true
		}
	}
	if finished != nil {
		if engine.Outcome(finished.Outcome) == engine.OutcomeOK {
			return false, fmt.Sprintf("awf resume: run %q already finished (ok). Nothing to resume.\n", runID), ""
		}
		return true, "", finished.Outcome
	}
	if cancelled {
		return true, "", "cancelled"
	}
	return true, "", "" // interrupted / crash-window → admit silently
}
