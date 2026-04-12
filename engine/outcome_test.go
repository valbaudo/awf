package engine

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/retry"
)

func TestClassifyOutcomeMatrix(t *testing.T) {
	// Per spec §6 table:
	//   - exit_code == 0 AND awfParseErr == nil → ok
	//   - exit_code in non_retryable_exit_codes → permanent_failure
	//   - exit_code != 0 otherwise → retryable_failure
	//   - callErr != nil → retryable_failure (launch / transport / timeout)
	//   - unparseable AWF_OUTPUT (awfParseErr != nil, exit==0) → retryable_failure
	parseErr := errors.New("AWF_OUTPUT failed schema validation")
	callErr := errors.New("backend exec transport failure")
	nonRet := []int{78}

	cases := []struct {
		name        string
		exit        int
		awfParseErr error
		callErr     error
		want        Outcome
	}{
		{"exit-0 + parsed", 0, nil, nil, OutcomeOK},
		{"exit-0 + unparseable", 0, parseErr, nil, OutcomeRetryableFailure},
		{"exit-1 (generic nonzero)", 1, nil, nil, OutcomeRetryableFailure},
		{"exit-78 (declared permanent)", 78, nil, nil, OutcomePermanentFailure},
		{"call error (transport)", 0, nil, callErr, OutcomeRetryableFailure},
		{"call error wins over exit", 1, nil, callErr, OutcomeRetryableFailure},
		{"call error wins over parse", 0, parseErr, callErr, OutcomeRetryableFailure},
		{"parse error wins over exit-0", 0, parseErr, nil, OutcomeRetryableFailure},
		{"exit-78 wins over parse error (deterministic priority)", 78, parseErr, nil, OutcomePermanentFailure},
		{"call error beats permanent exit (exit never observed)", 78, nil, callErr, OutcomeRetryableFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyOutcome(c.exit, c.awfParseErr, c.callErr, nonRet)
			if got != c.want {
				t.Errorf("ClassifyOutcome(exit=%d, parseErr=%v, callErr=%v, nonRet=%v) = %v, want %v",
					c.exit, c.awfParseErr, c.callErr, nonRet, got, c.want)
			}
		})
	}
}

func TestClassifyOutcomeReadsPolicy(t *testing.T) {
	// Smoke test the integration: retry.Policy.NonRetryableExitCodes is what
	// callers actually pass in; ClassifyOutcome doesn't reach for a global
	// default, it takes the slice as a parameter.
	p, err := retry.Merge(retry.Default, nil)
	if err != nil {
		t.Fatalf("retry.Merge: %v", err)
	}
	got := ClassifyOutcome(78, nil, nil, p.NonRetryableExitCodes)
	if got != OutcomePermanentFailure {
		t.Errorf("with Default policy, exit-78 = %v, want %v", got, OutcomePermanentFailure)
	}
}
