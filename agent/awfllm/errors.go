package awfllm

import (
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
