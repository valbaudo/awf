package awfllm

import (
	"github.com/valbaudo/awf/agent"
)

// AdapterRef must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "awf/llm"

// DefaultEnvAllowlist — env names forwarded as candidate API keys: OPENAI_API_KEY
// (default) and ANTHROPIC_API_KEY (provider: anthropic). Both overlap other
// adapters; defaultAgentEnv dedups (cli/agent_registry.go).
var DefaultEnvAllowlist = []string{defaultAPIKeyEnv, defaultAnthropicAPIKeyEnv}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig (permanent).
func wrapInvalidConfig(reason, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}
