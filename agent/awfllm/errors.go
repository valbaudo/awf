package awfllm

import (
	"fmt"
	"slices"

	"github.com/valbaudo/awf/agent"
)

// AdapterRef must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "awf/llm"

// DefaultEnvAllowlist — env names forwarded as candidate API keys. OPENAI_API_KEY
// overlaps goose/codex → defaultAgentEnv dedups (cli/agent_registry.go). A user
// pointing at a local server sets OPENAI_API_KEY to any non-empty placeholder.
var DefaultEnvAllowlist = []string{"OPENAI_API_KEY"}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig (permanent).
func wrapInvalidConfig(reason, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}

// ErrMissingAPIKey — the env var named by `api_key_env` (default OPENAI_API_KEY)
// is not present in the forwarded allowlist. Permanent: a missing credential
// won't appear on retry. The dispatch path classifies ValidateConfig errors as
// permanent_failure via *agent.ErrInvalidConfig (engine/local_dispatcher_agent.go).
type ErrMissingAPIKey struct {
	RequiredKey   string
	AvailableKeys []string
}

func (e *ErrMissingAPIKey) Error() string {
	return fmt.Sprintf("agent/awfllm: api_key_env %q not present in forwarded env (available: %v)", e.RequiredKey, slices.Clone(e.AvailableKeys))
}

// compile-time assertion that the error set satisfies error.
var _ = []error{
	(*ErrMissingAPIKey)(nil),
}
