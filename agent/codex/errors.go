package codex

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// AdapterRef must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "openai/codex"

// DefaultEnvAllowlist — env names the codex adapter forwards into each `codex exec`.
// Single source of truth for the CLI --agent-env default and resume's implicit
// allowlist. OPENAI_API_KEY overlaps goose's allowlist → defaultAgentEnv dedups
// (see cli/agent_registry.go). CODEX_HOME points codex at its auth.json/config dir
// (the ChatGPT-OAuth provisioning path). The adapter forces no env (--ephemeral
// handles session persistence).
var DefaultEnvAllowlist = []string{"OPENAI_API_KEY", "CODEX_HOME"}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig (permanent).
func wrapInvalidConfig(reason, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}

// ErrSessionReuseAttempted — a with-key that would re-use a codex session,
// breaking gate independence (spec §5.5). Defense-in-depth: the adapter always
// runs bare `codex exec`, never a resume/fork subcommand.
type ErrSessionReuseAttempted struct{ Key string }

func (e *ErrSessionReuseAttempted) Error() string {
	return fmt.Sprintf("agent/codex: with-key %q would re-use a codex session, breaking gate independence (spec §5.5)", e.Key)
}

// ErrStreamParse backs the parse unit tests; Launch tolerates a stray non-JSON line.
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
	return fmt.Sprintf("agent/codex: parse codex --json line %q: %v", preview, e.Cause)
}

func (e *ErrStreamParse) Unwrap() error { return e.Cause }

// ErrUnexpectedExit is sent (bare) when codex exited with no usable result. Output
// is the captured stdout/diag tail. The engine default branch maps it to
// retryable_failure.
type ErrUnexpectedExit struct {
	ExitCode int
	Output   string
}

func (e *ErrUnexpectedExit) Error() string {
	const maxOut = 300
	preview := e.Output
	if len(preview) > maxOut {
		preview = preview[:maxOut]
	}
	return fmt.Sprintf("agent/codex: codex exited with code %d and no usable result: %s", e.ExitCode, preview)
}

// ErrRuntimeNotFound — Version failed.
type ErrRuntimeNotFound struct {
	Ref       string
	Container string
	Cause     error
}

func (e *ErrRuntimeNotFound) Error() string {
	return fmt.Sprintf("agent/codex: %q binary not found in container %q: %v", e.Ref, e.Container, e.Cause)
}

func (e *ErrRuntimeNotFound) Unwrap() error { return e.Cause }

// Compile-time assertion that codex's error set satisfies error (codex omits
// goose's ErrMissingAPIKey/ErrAuthFailureSentinel — no provider-conditional auth
// gate, no fabricated sentinel; codex surfaces real turn.failed events).
var _ = []error{
	(*ErrSessionReuseAttempted)(nil),
	(*ErrStreamParse)(nil),
	(*ErrUnexpectedExit)(nil),
	(*ErrRuntimeNotFound)(nil),
}
