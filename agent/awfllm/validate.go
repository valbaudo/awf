package awfllm

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// with-config key names (shared by ValidateConfig and launch.go so they can
// never disagree).
const (
	keyModel            = "model"
	keyPrompt           = "prompt"
	keyBaseURL          = "base_url"
	keyAPIKeyEnv        = "api_key_env"
	keySystemPrompt     = "system_prompt"
	keyTemperature      = "temperature"
	keyMaxTokens        = "max_tokens"
	keyStructuredOutput = "structured_output"
	keyTLSInsecure      = "tls_insecure"
)

var allowedKeys = map[string]struct{}{
	keyModel: {}, keyPrompt: {}, keyBaseURL: {}, keyAPIKeyEnv: {},
	keySystemPrompt: {}, keyTemperature: {}, keyMaxTokens: {}, keyStructuredOutput: {},
	keyTLSInsecure: {},
}

// rejectedKeys never belong in `with:` — `api_key` would inline a secret into
// the durable definition; messages/tools/stream/session_id imply a surface this
// single-call adapter does not support (and would break gate independence).
var rejectedKeys = []string{"api_key", "session_id", "messages", "tools", "stream"}

// structuredOutputValues — the strategy enum (no `auto`: sniffing base_url is
// fragile). response_format = OpenAI-compat strict; ollama_format = native
// /api/chat format; off = prompt-only + tolerant parse.
var structuredOutputValues = []string{soResponseFormat, soOllamaFormat, soOff}

const defaultAPIKeyEnv = "OPENAI_API_KEY"

// ValidateConfig enforces the with-schema: reject-inline/session → unknown-key →
// required(model,prompt) → per-key types+enum → api-key-present. Deterministic
// (sorted keys); idempotent (run-start walk + defensive dispatch).
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	for _, k := range rejectedKeys {
		if _, present := with[k]; present {
			return wrapInvalidConfig("not supported (use api_key_env, and a single `prompt`)", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: k, Reason: fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys)))}
		}
	}
	if err := requireNonEmptyString(with, keyModel); err != nil {
		return err
	}
	if err := requireNonEmptyString(with, keyPrompt); err != nil {
		return err
	}
	for _, k := range []string{keyBaseURL, keyAPIKeyEnv, keySystemPrompt} {
		if v, ok := with[k]; ok {
			if _, ok := v.(string); !ok {
				return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), k)
			}
		}
	}
	if v, ok := with[keyTemperature]; ok && !isNumber(v) {
		return wrapInvalidConfig(fmt.Sprintf("must be a number, got %T", v), keyTemperature)
	}
	if v, ok := with[keyMaxTokens]; ok && !isInteger(v) {
		return wrapInvalidConfig(fmt.Sprintf("must be an integer, got %v", v), keyMaxTokens)
	}
	if v, ok := with[keyStructuredOutput]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyStructuredOutput)
		}
		if !slices.Contains(structuredOutputValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", structuredOutputValues, s), keyStructuredOutput)
		}
	}
	if v, ok := with[keyTLSInsecure]; ok {
		if _, ok := v.(bool); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be a bool, got %T", v), keyTLSInsecure)
		}
	}
	// Policy: the API key env var (default OPENAI_API_KEY) must be present.
	keyName := defaultAPIKeyEnv
	if v, ok := with[keyAPIKeyEnv].(string); ok && v != "" {
		keyName = v
	}
	if _, present := a.env[keyName]; !present {
		return wrapInvalidConfig(fmt.Sprintf("env var %q not present in the forwarded allowlist (available: %v)", keyName, slices.Sorted(maps.Keys(a.env))), keyAPIKeyEnv)
	}
	return nil
}

func requireNonEmptyString(with ir.RawConfig, key string) error {
	v, ok := with[key]
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: "required"}
	}
	s, ok := v.(string)
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: fmt.Sprintf("must be string, got %T", v)}
	}
	if s == "" {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: "must not be empty"}
	}
	return nil
}

// isNumber accepts the numeric types a YAML/JSON decoder may produce (temperature).
func isNumber(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	default:
		return false
	}
}

// isInteger accepts integer-valued numbers; rejects a non-integral float like 10.5
// (max_tokens must be whole — N6).
func isInteger(v any) bool {
	switch n := v.(type) {
	case int, int64:
		return true
	case float64:
		return n == float64(int64(n))
	default:
		return false
	}
}
