package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
)

func TestSnapshotFailureOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Outcome
	}{
		{"too-large is permanent", container.ErrSnapshotTooLarge, OutcomePermanentFailure},
		{"wrapped too-large is permanent", fmt.Errorf("snapshot ws: %w", container.ErrSnapshotTooLarge), OutcomePermanentFailure},
		{"unsupported is permanent", container.ErrUnsupported, OutcomePermanentFailure},
		{"transient is retryable", errors.New("docker daemon hiccup"), OutcomeRetryableFailure},
		{"wrapped transient is retryable", fmt.Errorf("x: %w", errors.New("io timeout")), OutcomeRetryableFailure},
	}
	for _, c := range cases {
		if got := snapshotFailureOutcome(c.err); got != c.want {
			t.Errorf("%s: snapshotFailureOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}
