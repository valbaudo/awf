package agent

import (
	"errors"
	"fmt"
	"time"
)

// ErrAdapterAlreadyRegistered is returned by Registry.Register when a second
// adapter is registered under the same Ref(). The Registry never silently
// overwrites — the caller (CLI start-time wiring) controls registration
// order and a duplicate signals a configuration bug.
type ErrAdapterAlreadyRegistered struct {
	Ref string
}

func (e *ErrAdapterAlreadyRegistered) Error() string {
	return fmt.Sprintf("agent: adapter %q already registered", e.Ref)
}

// ErrAdapterNotFound is returned by Resolver.Lookup's caller via wrapping
// when a workflow's `uses:` ref doesn't match any registered adapter.
// Lookup itself returns (nil, false); the caller (slice 5.2 dispatcher
// and this slice's CLI resolveRuntimes helper) wraps in this typed
// error for the top-level error path.
type ErrAdapterNotFound struct {
	Ref string
}

func (e *ErrAdapterNotFound) Error() string {
	return fmt.Sprintf("agent: no adapter registered for %q", e.Ref)
}

// ErrInvalidConfig is the result of Adapter.ValidateConfig rejection.
// Carries enough context to point at the offending workflow key in the
// CLI's diagnostic.
type ErrInvalidConfig struct {
	Ref    string // adapter ref (e.g. "anthropic/claude-code")
	Key    string // the with-key that caused the error (e.g. "session_id"); empty if the whole config is bad
	Reason string // human-readable reason

	// KeyUnknown is true when the rejection is an unknown with-key (a typo'd
	// or wrong-adapter key name). The run-start guard always surfaces these;
	// value-shape errors are suppressed for templated ("{{...}}") values.
	KeyUnknown bool
}

func (e *ErrInvalidConfig) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("agent: adapter %q rejected config: %s", e.Ref, e.Reason)
	}
	return fmt.Sprintf("agent: adapter %q rejected config key %q: %s", e.Ref, e.Key, e.Reason)
}

// ErrUnparseableOutput is returned by Adapter.Launch when the harness
// produced output that doesn't validate against AgentInvocation.OutputSchema.
// The slice 5.2 dispatcher maps this to retryable_failure (the engine's
// existing retry policy handles re-execution).
type ErrUnparseableOutput struct {
	NodePath string
}

func (e *ErrUnparseableOutput) Error() string {
	return fmt.Sprintf("agent: unparseable output at node %q", e.NodePath)
}

var (
	ErrLiveLeaseConflict   = errors.New("agent: live lease conflict")
	ErrLiveLeaseStaleOwned = errors.New("agent: live lease stale owned")
	ErrLiveRegistryIO      = errors.New("agent: live registry I/O")
	ErrLiveReplayRequired  = errors.New("agent: live replay required")
	ErrPermissionDenied    = errors.New("agent: permission denied")
	ErrLiveRedaction       = errors.New("agent: live redaction")
)

// RetryHint is an optional, server-derived "wait this long before retrying"
// signal an adapter may attach to a transient ErrAgentLaunch. RetryAfter is
// parsed from a provider Retry-After header or a rate-limit reset time (e.g.
// Anthropic anthropic-ratelimit-*-reset, or the Claude Code rate_limit event's
// resetsAt). The engine dispatcher lifts it onto DispatchResult.RetryAfter and
// engine.RunWithRetry honors it (capped at retry.MaxHonoredRetryAfter) so AWF
// waits out the actual window instead of the short exp curve. Runtime-only —
// never journaled.
type RetryHint struct {
	RetryAfter time.Duration
}

// ErrAgentLaunch is returned by Adapter.Launch when the harness failed to
// start or terminated abnormally (transport / launch class). Wraps the
// concrete cause for errors.Is matching at the top of the call chain.
// The slice 5.2 dispatcher maps this to retryable_failure.
//
// RetryHint is optional: when the cause is a provider rate-limit/overload with a
// known retry window, the adapter sets it and the engine honors that delay.
type ErrAgentLaunch struct {
	Cause     error
	RetryHint *RetryHint
}

func (e *ErrAgentLaunch) Error() string {
	return fmt.Sprintf("agent: launch failed: %v", e.Cause)
}

func (e *ErrAgentLaunch) Unwrap() error { return e.Cause }
