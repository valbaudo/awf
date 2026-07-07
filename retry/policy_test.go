package retry_test

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
)

func TestDefaultPolicy(t *testing.T) {
	d := retry.Default
	if d.Attempts != 8 {
		t.Errorf("Attempts = %d, want 8", d.Attempts)
	}
	if d.Backoff != retry.BackoffExp {
		t.Errorf("Backoff = %v, want exp", d.Backoff)
	}
	if d.Initial != time.Second {
		t.Errorf("Initial = %v, want 1s", d.Initial)
	}
	if d.Max != 60*time.Second {
		t.Errorf("Max = %v, want 60s", d.Max)
	}
	if len(d.NonRetryableExitCodes) != 1 || d.NonRetryableExitCodes[0] != 78 {
		t.Errorf("NonRetryableExitCodes = %v, want [78]", d.NonRetryableExitCodes)
	}
}

func TestMergeNilOverrideReturnsDefaultDeepCopy(t *testing.T) {
	// Revision #6: even on the no-override path, Merge MUST deep-copy the
	// slice fields so two callers can't observe each other's mutations.
	// Index-mutation on retry.Default.NonRetryableExitCodes was a real footgun
	// without this; verifying the copy isolates the seam where it matters.
	got, err := retry.Merge(retry.Default, nil)
	if err != nil {
		t.Fatalf("Merge(Default, nil): %v", err)
	}
	if got.Attempts != retry.Default.Attempts {
		t.Errorf("Attempts = %d, want %d", got.Attempts, retry.Default.Attempts)
	}
	// Deep-copy check: mutate got.NonRetryableExitCodes[0]; retry.Default must be unchanged.
	if len(got.NonRetryableExitCodes) != 1 {
		t.Fatalf("want NonRetryableExitCodes len 1, got %d", len(got.NonRetryableExitCodes))
	}
	got.NonRetryableExitCodes[0] = 999
	if retry.Default.NonRetryableExitCodes[0] != 78 {
		t.Errorf("Merge aliased Default's slice: Default.NonRetryableExitCodes[0] = %d, want 78 (deep-copy violated)", retry.Default.NonRetryableExitCodes[0])
	}
}

func TestMergePerStepWinsField(t *testing.T) {
	initial := ir.Duration(500 * time.Millisecond)
	maxd := ir.Duration(30 * time.Second)
	over := &ir.RetryPolicy{
		Attempts:              5,
		Backoff:               "linear",
		Initial:               &initial,
		Max:                   &maxd,
		NonRetryableExitCodes: []int{1, 2, 3},
	}
	got, err := retry.Merge(retry.Default, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.Attempts != 5 {
		t.Errorf("Attempts = %d, want 5", got.Attempts)
	}
	if got.Backoff != retry.BackoffLinear {
		t.Errorf("Backoff = %v, want linear", got.Backoff)
	}
	if got.Initial != 500*time.Millisecond {
		t.Errorf("Initial = %v, want 500ms", got.Initial)
	}
	if got.Max != 30*time.Second {
		t.Errorf("Max = %v, want 30s", got.Max)
	}
	// Slice fields: per-step REPLACES (does not append).
	if len(got.NonRetryableExitCodes) != 3 ||
		got.NonRetryableExitCodes[0] != 1 ||
		got.NonRetryableExitCodes[1] != 2 ||
		got.NonRetryableExitCodes[2] != 3 {
		t.Errorf("NonRetryableExitCodes = %v, want [1,2,3] (replace, not append)", got.NonRetryableExitCodes)
	}
}

func TestMergePartialOverrideKeepsDefaultsForUnsetFields(t *testing.T) {
	over := &ir.RetryPolicy{Attempts: 7} // only Attempts set
	got, err := retry.Merge(retry.Default, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.Attempts != 7 {
		t.Errorf("Attempts = %d, want 7", got.Attempts)
	}
	if got.Backoff != retry.Default.Backoff {
		t.Errorf("Backoff = %v, want default %v", got.Backoff, retry.Default.Backoff)
	}
	if got.Initial != retry.Default.Initial {
		t.Errorf("Initial = %v, want default %v", got.Initial, retry.Default.Initial)
	}
	if got.Max != retry.Default.Max {
		t.Errorf("Max = %v, want default %v", got.Max, retry.Default.Max)
	}
	if len(got.NonRetryableExitCodes) != 1 || got.NonRetryableExitCodes[0] != 78 {
		t.Errorf("NonRetryableExitCodes = %v, want default [78]", got.NonRetryableExitCodes)
	}
}

func TestMergeRecoveryOverridesBase(t *testing.T) {
	// Rk: a non-empty per-step recovery overrides the base value.
	got, err := retry.Merge(retry.Default, &ir.RetryPolicy{Recovery: "continue"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.Recovery != "continue" {
		t.Errorf("Recovery = %q, want continue (per-step override)", got.Recovery)
	}

	// An empty override leaves the base value in place (set-if-non-empty semantic).
	base := retry.Default
	base.Recovery = "restart"
	got2, err := retry.Merge(base, &ir.RetryPolicy{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got2.Recovery != "restart" {
		t.Errorf("Recovery = %q, want restart (base preserved on empty override)", got2.Recovery)
	}

	// Default leaves Recovery unset (the engine resolves the per-adapter default).
	if retry.Default.Recovery != "" {
		t.Errorf("Default.Recovery = %q, want empty (unset)", retry.Default.Recovery)
	}
}

func TestMergeUnknownBackoffIsError(t *testing.T) {
	// Revision #8: unknown backoff strings used to silently fall back to exp.
	// Slice 1.4 doesn't validate retry.backoff (verified by grepping
	// ir/validate*.go), so the runtime is the only line of defense. Until a
	// future Phase 1.x validator pass catches the typo, Merge errors hard.
	cases := []string{"expo", "exponential", "rand", "EXP"}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			over := &ir.RetryPolicy{Backoff: s}
			_, err := retry.Merge(retry.Default, over)
			if err == nil {
				t.Errorf("Merge with backoff=%q: err = nil, want unknown-backoff error", s)
			}
		})
	}
}

func TestBackoffForExp(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 10 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0}, // first attempt has no preceding sleep
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
		{6, 10 * time.Second}, // capped at Max
		{7, 10 * time.Second}, // still capped
	}
	for _, c := range cases {
		got := p.BackoffFor(c.attempt)
		if got != c.want {
			t.Errorf("BackoffFor(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestBackoffForLinear(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffLinear, Initial: 2 * time.Second, Max: 7 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 6 * time.Second},
		{5, 7 * time.Second}, // capped at Max
	}
	for _, c := range cases {
		got := p.BackoffFor(c.attempt)
		if got != c.want {
			t.Errorf("BackoffFor(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestBackoffForNone(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffNone, Initial: time.Second, Max: 60 * time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		if got := p.BackoffFor(attempt); got != 0 {
			t.Errorf("BackoffFor(%d) with BackoffNone = %v, want 0", attempt, got)
		}
	}
}

func TestIsRetryableExit(t *testing.T) {
	p := retry.Policy{NonRetryableExitCodes: []int{78, 130}}
	cases := []struct {
		code int
		want bool
	}{
		{0, false},   // success — not retryable (because already done)
		{1, true},    // generic failure — retryable
		{78, false},  // declared permanent
		{130, false}, // also declared permanent
		{99, true},   // not in list — retryable
	}
	for _, c := range cases {
		got := p.IsRetryableExit(c.code)
		if got != c.want {
			t.Errorf("IsRetryableExit(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}
