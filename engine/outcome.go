package engine

// ClassifyOutcome is the spec §6 mechanical mapping from a single dispatch
// attempt's raw inputs (exit code, AWF_OUTPUT parse outcome, Backend.Exec
// transport outcome) to the typed Outcome the engine carries forward.
//
// Priority order (deterministic — required because multiple causes can be
// non-nil at once on a chaotic call):
//  1. callErr  (transport / timeout / launch failure) → retryable_failure
//  2. exit ∈ nonRetryableExitCodes                    → permanent_failure
//  3. exit != 0                                       → retryable_failure
//  4. awfParseErr != nil  (exit 0, unparseable output)→ retryable_failure
//  5. exit == 0 and awfParseErr == nil                → ok
//
// The "permanent wins over parse error" priority is deliberate: if the author
// declared `non_retryable_exit_codes: [78]` and the step exited 78 with broken
// AWFOutput, the author's policy decision (permanent) trumps the parse
// detail. Authors who want a parse error to escalate to permanent_failure
// should not declare an exit code as permanent.
//
// Note: callErr beats permanent here. A transport failure (network error,
// container died unexpectedly) is mechanically a retryable_failure regardless
// of what exit code the step would have produced — we never saw the exit
// code; what we saw is the call failing. Spec §6 puts launch/transport in the
// retryable bucket.
func ClassifyOutcome(exitCode int, awfParseErr error, callErr error, nonRetryableExitCodes []int) Outcome {
	if callErr != nil {
		return OutcomeRetryableFailure
	}
	for _, c := range nonRetryableExitCodes {
		if exitCode == c {
			return OutcomePermanentFailure
		}
	}
	if exitCode != 0 {
		return OutcomeRetryableFailure
	}
	if awfParseErr != nil {
		return OutcomeRetryableFailure
	}
	return OutcomeOK
}
