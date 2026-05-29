package droid

import "fmt"

// AdapterRef is the agent-runtime identifier this package's Adapter returns
// from Ref(). Must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "factory/droid"

// DefaultEnvAllowlist is the canonical set of env-var names the droid adapter
// reads. Single source of truth for the CLI's --agent-env default and resume's
// implicit allowlist. droid authenticates headlessly only via FACTORY_API_KEY.
var DefaultEnvAllowlist = []string{
	"FACTORY_API_KEY",
}

// ErrMissingAPIKey is returned by ValidateConfig when FACTORY_API_KEY is absent
// from the adapter's env allowlist. droid has no headless auth fallback, so we
// fail at the run-start walk with an actionable message.
type ErrMissingAPIKey struct {
	AvailableKeys []string
}

func (e *ErrMissingAPIKey) Error() string {
	if len(e.AvailableKeys) == 0 {
		return "agent/droid: requires FACTORY_API_KEY in the adapter's env allowlist (--agent-env), but the allowlist is empty"
	}
	return fmt.Sprintf("agent/droid: requires FACTORY_API_KEY in the adapter's env allowlist (--agent-env); allowlist has %v but FACTORY_API_KEY is not present", e.AvailableKeys)
}

// ErrSessionReuseAttempted is returned by ValidateConfig when a with-key names
// a flag that would re-use a prior droid session (session_id / resume / fork /
// continue). Gate independence (spec §5.5) is engine-enforced, not convention.
type ErrSessionReuseAttempted struct {
	Key string
}

func (e *ErrSessionReuseAttempted) Error() string {
	return fmt.Sprintf("agent/droid: with-key %q would re-use a droid session, breaking gate independence (spec §5.5)", e.Key)
}

// ErrStreamParse is returned by parseEnvelope when a stdout line fails to decode
// as the -o json envelope. lastEnvelope treats an unparseable buffer as "no
// envelope" (→ the ErrUnexpectedExit / stderr-config-error path), so this type
// does not itself propagate out of Launch; it backs the parse unit tests.
type ErrStreamParse struct {
	Line  []byte
	Cause error
}

func (e *ErrStreamParse) Error() string {
	const maxLine = 200
	preview := e.Line
	if len(preview) > maxLine {
		preview = preview[:maxLine]
	}
	return fmt.Sprintf("agent/droid: parse droid -o json line %q: %v", preview, e.Cause)
}

func (e *ErrStreamParse) Unwrap() error { return e.Cause }

// ErrUnexpectedExit is sent (bare) on Launch's outcome channel when droid exited
// with no parseable result envelope on stdout AND stderr matched no known
// config-error pattern. The engine's classifier maps unrecognized error types to
// retryable_failure via its default branch, so a bare ErrUnexpectedExit is
// treated as transport-class (retryable) without an explicit ErrAgentLaunch wrap.
type ErrUnexpectedExit struct {
	ExitCode int
	Stderr   string
}

func (e *ErrUnexpectedExit) Error() string {
	const maxStderr = 300
	preview := e.Stderr
	if len(preview) > maxStderr {
		preview = preview[:maxStderr]
	}
	return fmt.Sprintf("agent/droid: droid exited with code %d and no result envelope: stderr=%q", e.ExitCode, preview)
}

// ErrRuntimeNotFound is returned by Version when `droid --version` failed.
type ErrRuntimeNotFound struct {
	Ref       string
	Container string
	Cause     error
}

func (e *ErrRuntimeNotFound) Error() string {
	return fmt.Sprintf("agent/droid: %q binary not found in container %q: %v", e.Ref, e.Container, e.Cause)
}

func (e *ErrRuntimeNotFound) Unwrap() error { return e.Cause }

// Keep-alive: also marks these types as used for the `unused` linter so they
// stay lint-clean in this commit before their callers land in later tasks.
var _ = []error{
	(*ErrMissingAPIKey)(nil),
	(*ErrSessionReuseAttempted)(nil),
	(*ErrStreamParse)(nil),
	(*ErrUnexpectedExit)(nil),
	(*ErrRuntimeNotFound)(nil),
}
