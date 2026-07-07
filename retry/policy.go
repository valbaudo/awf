// Package retry provides retry policy, backoff math, and per-policy exit-code
// classification — the data primitive that engine.RunWithRetry composes with a
// Dispatcher + Clock into the retry loop. No engine / state / clock deps here;
// the package is leaf-ward by design (Phase 3 will reuse it for try/catch
// integration without rewiring).
package retry

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/valbaudo/awf/ir"
)

// BackoffKind selects which curve BackoffFor uses to compute the sleep before
// attempt n. The string form is what ir.RetryPolicy.Backoff carries
// (spec §6 — "exp" / "linear" / ""); the typed constants here are the engine-
// internal form. Unknown strings are an error at Merge time (Revision #8) —
// slice 1.4 doesn't yet validate this field, so the runtime is the line of
// defense.
type BackoffKind int

const (
	BackoffExp    BackoffKind = iota // default — doubles each attempt, capped at Max
	BackoffLinear                    // adds Initial each attempt, capped at Max
	BackoffNone                      // always 0 (e.g. for tests where backoff would slow things down)
)

// backoffKindFromString parses a wire-format backoff name. Returns an error
// (not a silent fallback) on unknown strings — the spec doesn't sanction
// "expo" / "exponentialish" / etc., and silently treating typos as exp is
// the wrong fault-mode for a security-sensitive runtime (Revision #8).
//
// Until a future Phase 1.x validator pass catches this at validation time
// (filed as follow-up; not in slice 2.4 scope), the call sites surface the
// error to the operator at run start.
func backoffKindFromString(s string) (BackoffKind, error) {
	switch s {
	case "exp", "":
		return BackoffExp, nil
	case "linear":
		return BackoffLinear, nil
	case "none":
		return BackoffNone, nil
	default:
		return 0, fmt.Errorf("retry: unknown backoff kind %q (want \"exp\" | \"linear\" | \"none\" | \"\")", s)
	}
}

// Policy is the merged-down retry configuration the engine actually consults at
// dispatch time. ir.RetryPolicy is the wire/IR form (omitempty fields, string
// backoff name); Merge collapses default + per-step into this typed struct.
type Policy struct {
	Attempts              int
	Backoff               BackoffKind
	Initial               time.Duration
	Max                   time.Duration
	NonRetryableExitCodes []int
	// Recovery is the merged recovery strategy ("continue" | "restart" | "" =
	// unset). The wire form is ir.RetryPolicy.Recovery; Merge copies a non-empty
	// per-step value over the base. Default leaves it unset ("") — the engine
	// resolves the per-adapter effective value at dispatch time.
	Recovery string
}

// Default is the spec §6 default policy. The 78 sentinel is EX_CONFIG from
// sysexits.h — the conventional "configuration error, retry won't help" code.
// Treat as read-only — Merge deep-copies the NonRetryableExitCodes slice so
// callers can't accidentally mutate the shared default via index assignment
// (Revision #6 narrowed the var-vs-func footgun to the seam where it matters).
//
// Attempts is 8 (not 3) so transient provider faults — Anthropic 429
// rate_limit_error, 529 overloaded_error, 5xx, connection drops — ride out a
// normal rate-limit window or overload without failing the pipeline. With the
// exp curve capped at Max (1,2,4,8,16,32,60 → ~123s of backoff across 7 sleeps)
// plus any honored Retry-After hint, 8 attempts covers the common outage; a
// genuinely permanent fault (bad key, invalid_request, quota exhausted) is
// classified permanent_failure upstream and never consumes the budget.
var Default = Policy{
	Attempts:              8,
	Backoff:               BackoffExp,
	Initial:               time.Second,
	Max:                   60 * time.Second,
	NonRetryableExitCodes: []int{78},
}

// MaxHonoredRetryAfter caps how long a single server-supplied Retry-After hint
// can stretch one sleep. Honoring a hint verbatim is correct (the server knows
// its own reset window) up to a point — a buggy proxy or upstream that returns
// "retry-after: 86400" must not be allowed to hang the pipeline for a day. Real
// Anthropic 429/529 hints are seconds to low minutes; 5m is a generous ceiling
// that rides out a true overload while bounding the worst case.
const MaxHonoredRetryAfter = 5 * time.Minute

// jitterFraction is the maximum additive jitter as a fraction of the base sleep
// (25%, matching the Anthropic SDK). Jitter is additive-only so the sleep never
// undershoots the curve or an honored Retry-After.
const jitterFraction = 0.25

// Merge collapses def + an optional per-step override into a single Policy.
// Per spec §6 + plan Design question 6: per-step wins field-by-field; slice
// fields replace entirely (the field IS the slice, not a partial overlay).
//
// A nil override still does work — it returns a deep-copy of def so the
// caller's returned Policy has its own NonRetryableExitCodes slice (Revision
// #6: protects against `p, _ := Merge(Default, nil); p.NonRetryableExitCodes[0]
// = 999` mutating Default for the next caller). A non-nil override's
// zero-valued fields (Attempts == 0, Initial == nil, etc.) leave the
// default in place — the override semantic is "set if non-zero" so the IR's
// `omitempty` shape doesn't silently zero a field the author didn't write.
//
// Returns an error if override.Backoff is an unknown string (Revision #8) —
// callers MUST handle this; slice 2.5's interpreter halts the run with the
// error message at run-start, before the first dispatch.
func Merge(def Policy, override *ir.RetryPolicy) (Policy, error) {
	out := def
	// Deep-copy default's slice field unconditionally (no-override + override-without-
	// NonRetryable both reach this path).
	if def.NonRetryableExitCodes != nil {
		dup := make([]int, len(def.NonRetryableExitCodes))
		copy(dup, def.NonRetryableExitCodes)
		out.NonRetryableExitCodes = dup
	}
	if override == nil {
		return out, nil
	}
	if override.Attempts != 0 {
		out.Attempts = override.Attempts
	}
	if override.Backoff != "" {
		k, err := backoffKindFromString(override.Backoff)
		if err != nil {
			return Policy{}, err
		}
		out.Backoff = k
	}
	if override.Initial != nil {
		out.Initial = time.Duration(*override.Initial)
	}
	if override.Max != nil {
		out.Max = time.Duration(*override.Max)
	}
	if override.NonRetryableExitCodes != nil {
		// REPLACE (per Design question 6), not append.
		dup := make([]int, len(override.NonRetryableExitCodes))
		copy(dup, override.NonRetryableExitCodes)
		out.NonRetryableExitCodes = dup
	}
	if override.Recovery != "" {
		out.Recovery = override.Recovery
	}
	return out, nil
}

// BackoffFor returns the sleep duration to apply BEFORE attempt n (1-based).
// Attempt 1 always returns 0 (no preceding sleep). The curve is selected by
// p.Backoff and capped at p.Max:
//   - Exp:    p.Initial × 2^(n-2)  (attempt 2 = Initial, attempt 3 = 2×Initial, …)
//   - Linear: p.Initial × (n-1)    (attempt 2 = Initial, attempt 3 = 2×Initial, …)
//   - None:   0 unconditionally
//
// The engine's retry loop calls clk.Sleep(p.BackoffFor(attempt)) between
// dispatch calls.
func (p Policy) BackoffFor(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	var d time.Duration
	switch p.Backoff {
	case BackoffNone:
		return 0
	case BackoffLinear:
		d = p.Initial * time.Duration(attempt-1)
	case BackoffExp:
		fallthrough
	default:
		// Exp: Initial × 2^(attempt-2). Done with a left shift to keep the math
		// integer and avoid float drift.
		shift := uint(attempt - 2)
		// Guard against ridiculous attempt counts overflowing the shift (>62 bits).
		if shift >= 62 {
			d = p.Max
		} else {
			d = p.Initial << shift
		}
	}
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	return d
}

// EffectiveBackoff is the sleep the engine retry loop applies before an attempt
// (1-based). It composes three things, in order:
//
//  1. the curve (BackoffFor) — exp/linear/none, capped at Max;
//  2. an optional server retryAfter hint (from a Retry-After header or a
//     rate-limit reset time, surfaced via DispatchResult.RetryAfter) — when it
//     exceeds the curve the loop waits the longer of the two, so a 1s curve
//     doesn't burn attempts hammering a window the server said resets in 30s.
//     The honored hint is capped at MaxHonoredRetryAfter;
//  3. deterministic additive jitter in [0, jitterFraction·base) keyed on
//     (seed, attempt), so parallel retries against the same window decorrelate.
//
// seed is the node addressing path (engine NodeIntent.Path) — a pure function of
// the node graph (CLAUDE.md "node addressing is one pure function"), so the
// jitter is deterministic and resume-safe: it perturbs only the sleep duration,
// never the step inputs/outputs, and sleep durations are not journaled. No
// time.Now()/rand — the determinism invariant holds.
//
// Jitter is additive-only, so the result is always >= max(curve, hint): we never
// undershoot a server's Retry-After.
func (p Policy) EffectiveBackoff(attempt int, retryAfter time.Duration, seed string) time.Duration {
	base := p.BackoffFor(attempt)
	if retryAfter > 0 {
		if retryAfter > MaxHonoredRetryAfter {
			retryAfter = MaxHonoredRetryAfter
		}
		if retryAfter > base {
			base = retryAfter
		}
	}
	if base <= 0 {
		// attempt 1 (no preceding sleep) or BackoffNone with no hint — no jitter
		// on zero.
		return 0
	}
	return base + jitterFor(base, seed, attempt)
}

// jitterFor returns a deterministic jitter in [0, jitterFraction·base) derived
// from a SHA-256 of (seed, attempt). Pure: same inputs → same output, across
// runs and resumes. No rand.
func jitterFor(base time.Duration, seed string, attempt int) time.Duration {
	h := sha256.Sum256([]byte(seed + ":" + strconv.Itoa(attempt)))
	// First 8 bytes → uniform fraction in [0, 1].
	frac := float64(binary.BigEndian.Uint64(h[:8])) / float64(math.MaxUint64)
	return time.Duration(frac * jitterFraction * float64(base))
}

// IsRetryableExit reports whether a nonzero exit code triggers a retry under
// this policy. exit==0 returns false (success — nothing to retry). exit in
// NonRetryableExitCodes returns false (permanent — declared by the author).
// Everything else returns true (generic failure → retry per policy).
//
// The engine's classifier (engine.ClassifyOutcome) reads this to decide
// retryable_failure vs permanent_failure per spec §6.
func (p Policy) IsRetryableExit(exitCode int) bool {
	if exitCode == 0 {
		return false
	}
	for _, c := range p.NonRetryableExitCodes {
		if c == exitCode {
			return false
		}
	}
	return true
}
