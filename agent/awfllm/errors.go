package awfllm

import (
	"github.com/valbaudo/awf/agent"
)

// AdapterRef must match the workflow `uses:` literal byte-for-byte.
const AdapterRef = "awf/llm"

// DefaultEnvAllowlist — env names forwarded as candidate API keys: OPENAI_API_KEY
// (default), ANTHROPIC_API_KEY (provider: anthropic), and GEMINI_API_KEY
// (provider: gemini). OPENAI_API_KEY and ANTHROPIC_API_KEY overlap other
// adapters; defaultAgentEnv dedups (cli/agent_registry.go).
var DefaultEnvAllowlist = []string{defaultAPIKeyEnv, defaultAnthropicAPIKeyEnv, defaultGeminiAPIKeyEnv}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig (permanent).
func wrapInvalidConfig(reason, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}
