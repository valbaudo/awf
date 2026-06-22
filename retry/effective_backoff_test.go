package retry_test

import (
	"testing"
	"time"

	"github.com/valbaudo/awf/retry"
)

// EffectiveBackoff is the sleep the retry loop actually applies before an
// attempt: the curve (BackoffFor) raised to a server-supplied Retry-After hint
// when present, plus a bounded deterministic jitter so parallel retries against
// the same rate-limit window decorrelate. Jitter is additive in [0, 25%) so the
// sleep is NEVER shorter than the curve/hint (we must not undershoot a server's
// Retry-After) and at most 1.25× it.

func within(t *testing.T, got, base time.Duration) {
	t.Helper()
	if got < base {
		t.Errorf("EffectiveBackoff = %v, want >= base %v (jitter must be additive, never undershoot)", got, base)
	}
	if got > base+base/4 {
		t.Errorf("EffectiveBackoff = %v, want <= base*1.25 (%v)", got, base+base/4)
	}
}

func TestEffectiveBackoffCurveWhenNoHint(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	// attempt 3 curve = 2s; with no hint, base is the curve.
	got := p.EffectiveBackoff(3, 0, "node/path")
	within(t, got, 2*time.Second)
}

func TestEffectiveBackoffHonorsRetryAfterAboveCurve(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	// attempt 2 curve = 1s, but the server said wait 30s — honor the larger.
	got := p.EffectiveBackoff(2, 30*time.Second, "node/path")
	within(t, got, 30*time.Second)
}

func TestEffectiveBackoffKeepsCurveWhenHintSmaller(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	// attempt 5 curve = 8s; a 1s hint is smaller, so the curve wins.
	got := p.EffectiveBackoff(5, 1*time.Second, "node/path")
	within(t, got, 8*time.Second)
}

func TestEffectiveBackoffCapsPathologicalRetryAfter(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	// A buggy proxy says wait an hour. Honoring it literally would hang the
	// pipeline, so the honored hint is capped at MaxHonoredRetryAfter.
	got := p.EffectiveBackoff(2, time.Hour, "node/path")
	within(t, got, retry.MaxHonoredRetryAfter)
}

func TestEffectiveBackoffIsDeterministic(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	a := p.EffectiveBackoff(3, 0, "map[2].body")
	b := p.EffectiveBackoff(3, 0, "map[2].body")
	if a != b {
		t.Errorf("EffectiveBackoff not deterministic for same (attempt, hint, seed): %v != %v", a, b)
	}
}

func TestEffectiveBackoffDecorrelatesAcrossSeeds(t *testing.T) {
	// Two parallel map items at the same attempt must not sleep for the
	// identical duration, or they re-collide on the same rate-limit window.
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	a := p.EffectiveBackoff(3, 0, "map[0].body")
	b := p.EffectiveBackoff(3, 0, "map[1].body")
	if a == b {
		t.Errorf("EffectiveBackoff did not decorrelate distinct seeds: both %v", a)
	}
}

func TestEffectiveBackoffJitterVariesByAttempt(t *testing.T) {
	// Same node, consecutive attempts should not draw identical jitter — the
	// seed must fold in the attempt number, not just the path.
	p := retry.Policy{Backoff: retry.BackoffNone, Initial: time.Second, Max: 60 * time.Second}
	// Force a non-zero base via a hint so jitter is observable under BackoffNone.
	a := p.EffectiveBackoff(2, 10*time.Second, "node/path")
	b := p.EffectiveBackoff(3, 10*time.Second, "node/path")
	if a == b {
		t.Errorf("EffectiveBackoff jitter identical across attempts: both %v (seed must include attempt)", a)
	}
}

func TestEffectiveBackoffZeroWhenNoCurveNoHint(t *testing.T) {
	// BackoffNone with no server hint → no sleep at all (and no jitter on zero).
	p := retry.Policy{Backoff: retry.BackoffNone, Initial: time.Second, Max: 60 * time.Second}
	if got := p.EffectiveBackoff(3, 0, "node/path"); got != 0 {
		t.Errorf("EffectiveBackoff = %v, want 0 (BackoffNone, no hint)", got)
	}
}

func TestEffectiveBackoffFirstAttemptNoSleep(t *testing.T) {
	p := retry.Policy{Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}
	if got := p.EffectiveBackoff(1, 0, "node/path"); got != 0 {
		t.Errorf("EffectiveBackoff(1, ...) = %v, want 0 (attempt 1 has no preceding sleep)", got)
	}
}
