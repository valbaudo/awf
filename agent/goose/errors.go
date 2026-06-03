package goose

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// AdapterRef must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "block/goose"

// DefaultEnvAllowlist — env names the goose adapter reads. Single source of truth
// for the CLI --agent-env default and resume's implicit allowlist. The forced
// opsec knobs (GOOSE_MODE, GOOSE_DISABLE_KEYRING, GOOSE_TELEMETRY_ENABLED, XDG_*)
// are INJECTED by Launch, not allowlisted. ANTHROPIC_API_KEY overlaps claude's
// allowlist → defaultAgentEnv dedups (see cli/agent_registry.go).
var DefaultEnvAllowlist = []string{"GOOSE_PROVIDER", "GOOSE_MODEL", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig (permanent).
func wrapInvalidConfig(reason, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}

// ErrMissingAPIKey — ValidateConfig's provider-conditional gate (anthropic/openai)
// when GOOSE_PROVIDER is set in the adapter env but the matching key is absent.
type ErrMissingAPIKey struct{ Key string }

func (e *ErrMissingAPIKey) Error() string {
	return fmt.Sprintf("agent/goose: the configured GOOSE_PROVIDER requires %s in the adapter's env allowlist (--agent-env), but it is not present", e.Key)
}

// ErrSessionReuseAttempted — a with-key that would re-use a goose session.
type ErrSessionReuseAttempted struct{ Key string }

func (e *ErrSessionReuseAttempted) Error() string {
	return fmt.Sprintf("agent/goose: with-key %q would re-use a goose session, breaking gate independence (spec §5.5)", e.Key)
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
	return fmt.Sprintf("agent/goose: parse goose stream-json line %q: %v", preview, e.Cause)
}

func (e *ErrStreamParse) Unwrap() error { return e.Cause }

// ErrUnexpectedExit is sent (bare) when goose exited with no usable result. Output
// is the captured STDOUT tail — goose writes diagnostics to stdout; stderr is
// always empty. The engine default branch maps it to retryable_failure.
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
	return fmt.Sprintf("agent/goose: goose exited with code %d and no usable result: %s", e.ExitCode, preview)
}

// ErrRuntimeNotFound — Version failed.
type ErrRuntimeNotFound struct {
	Ref       string
	Container string
	Cause     error
}

func (e *ErrRuntimeNotFound) Error() string {
	return fmt.Sprintf("agent/goose: %q binary not found in container %q: %v", e.Ref, e.Container, e.Cause)
}

func (e *ErrRuntimeNotFound) Unwrap() error { return e.Cause }

var _ = []error{
	(*ErrMissingAPIKey)(nil),
	(*ErrSessionReuseAttempted)(nil),
	(*ErrStreamParse)(nil),
	(*ErrUnexpectedExit)(nil),
	(*ErrRuntimeNotFound)(nil),
}
