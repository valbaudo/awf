package claude

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// ErrBareRequiresAPIKey is returned by ValidateConfig when `with.bare`
// is true (the AWF reproducibility default per Phase 5 design decision 9)
// but neither ANTHROPIC_API_KEY nor ANTHROPIC_AUTH_TOKEN was registered
// in the adapter's env allowlist via WithEnv. Decision 15: bare mode
// disables Keychain + credentials-file + CLAUDE_CODE_OAUTH_TOKEN, so
// the only auth paths that work are the two API-key env vars.
//
// Rejecting at ValidateConfig (rather than at Launch's "Not logged in"
// failure) gives the workflow author an early, actionable error.
type ErrBareRequiresAPIKey struct {
	AvailableKeys []string // the keys currently registered (helpful for debugging)
}

func (e *ErrBareRequiresAPIKey) Error() string {
	if len(e.AvailableKeys) == 0 {
		return "agent/claude: with.bare: true requires ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the adapter's env allowlist (--agent-env), but the allowlist is empty"
	}
	return fmt.Sprintf("agent/claude: with.bare: true requires ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the adapter's env allowlist (--agent-env); allowlist has %v but neither key is present", e.AvailableKeys)
}

// ErrSessionReuseAttempted is returned by ValidateConfig when a with-key
// names a flag that would re-use a prior claude session
// (session_id / continue / resume). Phase 5 design decision 7 makes
// gate independence engine-enforced rather than convention-enforced;
// this is the validation surface.
type ErrSessionReuseAttempted struct {
	Key string // the offending with-key (one of session_id, continue, resume)
}

func (e *ErrSessionReuseAttempted) Error() string {
	return fmt.Sprintf("agent/claude: with-key %q would re-use a claude session, breaking gate independence (Phase 5 design decision 7 / spec §5.5)", e.Key)
}

// ErrStreamParse is returned by Launch when a stream-json line failed to
// decode. Wrapped into *agent.ErrAgentLaunch by the caller (transport
// class — retryable). Carries the raw line bytes (truncated) for the
// operator's diagnostic.
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
	return fmt.Sprintf("agent/claude: parse stream-json line %q: %v", preview, e.Cause)
}

func (e *ErrStreamParse) Unwrap() error { return e.Cause }

// ErrUnexpectedExit is returned by Launch when claude exited (chunks
// channel closed, result delivered) but no `result` event was observed
// in the stream — typically a non-zero exit before claude could emit
// its final structured result. Wrapped into *agent.ErrAgentLaunch by
// the caller.
type ErrUnexpectedExit struct {
	ExitCode int
	Stderr   string
}

func (e *ErrUnexpectedExit) Error() string {
	const maxStderr = 200
	preview := e.Stderr
	if len(preview) > maxStderr {
		preview = preview[:maxStderr]
	}
	return fmt.Sprintf("agent/claude: claude exited with code %d before emitting a result event: stderr=%q", e.ExitCode, preview)
}

// ErrAgentRuntimeNotFound is returned by Version when `claude --version`
// failed (binary missing on PATH inside the container's exec environment,
// or exited non-zero). The Ref + Container fields locate the failure
// in run-start diagnostics.
type ErrAgentRuntimeNotFound struct {
	Ref       string
	Container string
	Cause     error
}

func (e *ErrAgentRuntimeNotFound) Error() string {
	return fmt.Sprintf("agent/claude: %q binary not found in container %q: %v", e.Ref, e.Container, e.Cause)
}

func (e *ErrAgentRuntimeNotFound) Unwrap() error { return e.Cause }

// Compile-time assertion that all errors satisfy agent's error-class
// expectations (engine maps them per the Adapter contract; see
// agent/adapter.go Launch doc-comment for the table).
var _ = []error{
	(*ErrBareRequiresAPIKey)(nil),
	(*ErrSessionReuseAttempted)(nil),
	(*ErrStreamParse)(nil),
	(*ErrUnexpectedExit)(nil),
	(*ErrAgentRuntimeNotFound)(nil),
}

// agent.ErrInvalidConfig wrapping: ValidateConfig converts
// *ErrSessionReuseAttempted / *ErrBareRequiresAPIKey to the engine-side
// typed error via errors.As-friendly wrappers below.
func wrapInvalidConfig(reason string, key string) error {
	return &agent.ErrInvalidConfig{
		Ref:    AdapterRef,
		Key:    key,
		Reason: reason,
	}
}

// AdapterRef is the agent-runtime identifier this package's Adapter
// returns from Ref(). Constant so cli/agent_registry.go and unit tests
// can refer to it without typing the string literal.
const AdapterRef = "anthropic/claude-code"
