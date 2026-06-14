package obs

import (
	"encoding/json"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// RunStatus is the lifecycle status awf ls surfaces for a run.
type RunStatus string

const (
	RunRunning   RunStatus = "running"   // resolved by the CLI: incomplete + live lock holder
	RunPaused    RunStatus = "paused"    // run.paused, not subsequently resumed
	RunFinished  RunStatus = "finished"  // run.finished{ok}
	RunFailed    RunStatus = "failed"    // run.finished{permanent/rejected} OR terminal node.failed{permanent}
	RunResumable RunStatus = "resumable" // run.finished{retryable_failure} OR terminal node.failed{retryable_failure} — re-drivable via `awf resume`
	RunCancelled RunStatus = "cancelled" // run.cancelled (terminal)
	RunCrashed   RunStatus = "crashed"   // resolved by the CLI: incomplete + no live lock holder
	// RunIncomplete = started, no terminal event. DeriveStatus stops here; the
	// CLI resolves it to RunRunning (lock held) or RunCrashed (lock free) via the
	// sidecar flock probe (cli/runlock.go). DeriveStatus itself is pure / no OS.
	RunIncomplete RunStatus = "incomplete"
)

// DeriveStatus folds a run's log into a lifecycle status from the LOG ALONE —
// no liveness probe (the CLI adds that). It never returns RunRunning or
// RunCrashed; the "started, no terminal event" case is RunIncomplete, which the
// CLI splits via the lock. Deterministic and OS-free, mirroring obs.Project's
// pure-projection contract.
//
// Precedence (matches the spec's awf ls definition + crash≠verdict invariant):
//
//	run.cancelled                         → cancelled
//	run.finished{ok}                      → finished
//	run.finished{retryable_failure}       → resumable
//	run.finished{permanent/rejected}      → failed
//	last event node.failed{retryable}     → resumable
//	last event node.failed{permanent}     → failed
//	latest run-control event is run.paused→ paused
//	otherwise                             → incomplete  (CLI: running | crashed)
func DeriveStatus(events []state.Event) RunStatus {
	paused := false
	for _, e := range events {
		switch e.Type {
		case engine.EventRunCancelled:
			return RunCancelled
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err == nil {
				switch engine.Outcome(d.Outcome) {
				case engine.OutcomeOK:
					return RunFinished
				case engine.OutcomeRetryableFailure:
					return RunResumable
				}
			}
			return RunFailed
		case engine.EventRunPaused:
			paused = true
		case engine.EventRunResumed:
			paused = false
		}
	}
	// A node.failed as the LAST event ⇒ failed, NOT crashed. Verified: failStep
	// (engine/interpreter.go) syncs node.failed then returns the outcome, which
	// runAndFinish (cli/execute.go) always turns into run.finished{failed}; and a
	// CAUGHT failure is followed by the catch block's events. So in any non-crash
	// run, node.failed is NEVER the last event — it is last ONLY when the process
	// died after durably recording a real, permanent failure verdict. That is a
	// verdict (the run DID fail), not an in-flight crash, so `failed` is correct
	// and spec-aligned (the lock arbitrates only the genuinely in-flight case:
	// node.started with no terminal node event → incomplete → running|crashed).
	// Do NOT "simplify" this to crashed — that would discard a recorded verdict.
	if n := len(events); n > 0 && events[n-1].Type == engine.EventNodeFailed {
		var d engine.NodeFailedData
		if err := json.Unmarshal(events[n-1].Data, &d); err == nil &&
			engine.Outcome(d.Outcome) == engine.OutcomeRetryableFailure {
			return RunResumable
		}
		return RunFailed
	}
	if paused {
		return RunPaused
	}
	return RunIncomplete
}
