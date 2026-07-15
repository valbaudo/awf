package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRunResultPair(t *testing.T) {
	cause := errors.New("cause")
	tests := []struct {
		name        string
		outcome     Outcome
		err         error
		wantInvalid bool
	}{
		{name: "ok", outcome: OutcomeOK},
		{name: "retryable", outcome: OutcomeRetryableFailure, err: cause},
		{name: "permanent", outcome: OutcomePermanentFailure, err: cause},
		{name: "rejected", outcome: OutcomeRejected, err: cause},
		{name: "internal", err: cause},
		{name: "ok with error", outcome: OutcomeOK, err: cause, wantInvalid: true},
		{name: "failure without error", outcome: OutcomeRetryableFailure, wantInvalid: true},
		{name: "empty without error", wantInvalid: true},
		{name: "unknown", outcome: Outcome("mystery"), err: cause, wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oc, err := validateRunResultPair(tt.outcome, tt.err)
			if tt.wantInvalid {
				if oc != "" || err == nil || !strings.Contains(err.Error(), "invariant") {
					t.Fatalf("validate = (%q, %v), want empty invariant error", oc, err)
				}
				return
			}
			if oc != tt.outcome || err != tt.err {
				t.Fatalf("validate = (%q, %v), want (%q, %v)", oc, err, tt.outcome, tt.err)
			}
		})
	}
}
