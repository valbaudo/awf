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
	keyProvider         = "provider"
	keyMaxInlineBytes   = "max_inline_bytes"
)

var allowedKeys = map[string]struct{}{
	keyModel: {}, keyPrompt: {}, keyBaseURL: {}, keyAPIKeyEnv: {},
	keySystemPrompt: {}, keyTemperature: {}, keyMaxTokens: {}, keyStructuredOutput: {},
	keyTLSInsecure: {}, keyProvider: {}, keyMaxInlineBytes: {},
}

// rejectedKeys never belong in `with:` — `api_key` would inline a secret into
// the durable definition; messages/tools/stream/session_id imply a surface this
// single-call adapter does not support (and would break gate independence).
var rejectedKeys = []string{"api_key", "session_id", "messages", "tools", "stream"}

// structuredOutputValues — the strategy enum (no `auto`: sniffing base_url is
// fragile). response_format = OpenAI-compat strict; ollama_format = native
// /api/chat format; off = prompt-only + tolerant parse.
var structuredOutputValues = []string{soResponseFormat, soOllamaFormat, soOff}

// providerValues — the transport selector enum. Gates ONLY the native Gemini path;
// openai (the default) routes through the existing OpenAI/Ollama logic. Ollama is
// deliberately NOT a provider value: it stays on structured_output: ollama_format.
var providerValues = []string{providerOpenAI, providerGemini}

// defaultAPIKeyEnv is the canonical default API-key env-var name for this adapter.
// DefaultEnvAllowlist (errors.go) is built from this constant so the two values
// cannot drift independently.
const defaultAPIKeyEnv = "OPENAI_API_KEY"

// ValidateConfig enforces the with-schema: reject-inline/session → unknown-key →
// required(model,prompt) → per-key types+enum → api-key-present. Deterministic
// (sorted keys); idempotent (run-start walk + defensive dispatch).
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	if err := a.validateConfigCommon(with); err != nil {
		return err
	}
	return requireNonEmptyString(with, keyPrompt)
}

// validateConfigForToolLoop is ValidateConfig minus the prompt requirement: a
// react: step supplies the initial user turn at the step level and the engine owns
// the messages array, so `prompt` does not belong in a react with:. Everything else
// — the rejectedKeys guard (incl. "tools"), the unknown-key check, required(model),
// the per-key types/enum, and the api-key-present policy — still applies (Task 3.3).
func (a *Adapter) validateConfigForToolLoop(with ir.RawConfig) error {
	return a.validateConfigCommon(with)
}

// validateConfigCommon holds the shared body — everything except the keyPrompt
// require: rejectedKeys, allowedKeys, required(model), the per-key types/enum
// (base_url/api_key_env/system_prompt/temperature/max_tokens/max_inline_bytes/
// provider/structured_output/tls_insecure), and the api-key-env presence policy.
// ValidateConfig adds the prompt require on top; validateConfigForToolLoop does not.
func (a *Adapter) validateConfigCommon(with ir.RawConfig) error {
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
	if v, ok := with[keyMaxInlineBytes]; ok {
		if !isInteger(v) {
			return wrapInvalidConfig(fmt.Sprintf("must be an integer, got %v", v), keyMaxInlineBytes)
		}
		if n, _ := toInt(v); n <= 0 {
			return wrapInvalidConfig(fmt.Sprintf("must be a positive integer, got %v", v), keyMaxInlineBytes)
		}
	}
	if v, ok := with[keyProvider]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyProvider)
		}
		if !slices.Contains(providerValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", providerValues, s), keyProvider)
		}
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
	// Policy: the API key env var must be present. The default name tracks the
	// provider (GEMINI_API_KEY for gemini, OPENAI_API_KEY otherwise) so a valid
	// provider: gemini config relying on the default key env passes here too —
	// keeping this presence check consistent with buildReqConfig's resolution.
	keyName := defaultAPIKeyEnv
	if p, ok := with[keyProvider].(string); ok && p == providerGemini {
		keyName = defaultGeminiAPIKeyEnv
	}
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
