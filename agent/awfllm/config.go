package awfllm

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// defaultBaseURL is the OpenAI Chat Completions endpoint. When base_url is
// omitted the adapter targets OpenAI directly — a common footgun for users
// pointing at a local endpoint (Ollama, vLLM, llama.cpp): they must set base_url
// explicitly or their traffic will hit OpenAI, not the local server.
const defaultBaseURL = "https://api.openai.com/v1"

// structured_output mode constants — the accepted values for the
// structured_output with-key. Defined once here so validate.go and transport.go
// share a single source of truth instead of comparing against raw string literals.
const (
	soResponseFormat = "response_format" // OpenAI-compat strict response_format
	soOllamaFormat   = "ollama_format"   // Ollama native /api/chat format field
	soOff            = "off"             // prompt-only + tolerant layer-2 parse
)

// reqConfig is the per-call request shape built in Launch from validated `with:`.
type reqConfig struct {
	BaseURL          string
	APIKey           string
	Model            string
	SystemPrompt     string
	Temperature      float64
	HasTemperature   bool
	MaxTokens        int
	HasMaxTokens     bool
	StructuredOutput string // response_format | ollama_format | off
	IdempotencyKey   string
	TLSInsecure      bool // opt-in: skip TLS verification (self-signed/internal endpoints — offensive use)
}

// buildReqConfig translates validated `with:` + the resolved env into a reqConfig.
func (a *Adapter) buildReqConfig(inv agent.AgentInvocation) (reqConfig, error) {
	with := inv.With
	cfg := reqConfig{
		Model:            stringOr(with, keyModel, ""),
		BaseURL:          stringOr(with, keyBaseURL, defaultBaseURL),
		SystemPrompt:     stringOr(with, keySystemPrompt, ""),
		StructuredOutput: stringOr(with, keyStructuredOutput, soResponseFormat),
		IdempotencyKey:   inv.IdempotencyKey,
		TLSInsecure:      boolOr(with, keyTLSInsecure, false),
	}
	keyName := stringOr(with, keyAPIKeyEnv, defaultAPIKeyEnv)
	key, ok := a.env[keyName]
	if !ok {
		// Defensive (ValidateConfig already checked). Return the PERMANENT-classified
		// type: *agent.ErrInvalidConfig → permanent.
		return reqConfig{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyAPIKeyEnv, Reason: "named env var not present in the forwarded allowlist"}
	}
	cfg.APIKey = key
	// Do NOT default temperature: reasoning models (o1/o3/gpt-5) REJECT `temperature`
	// with a 400 invalid_request_error → classified PERMANENT. Send it only when the
	// author explicitly set it. (Recommend temperature:0 for local grammar paths in docs.)
	if v, ok := with[keyTemperature]; ok {
		cfg.Temperature, cfg.HasTemperature = toFloat(v)
	}
	if v, ok := with[keyMaxTokens]; ok {
		if n, okN := toInt(v); okN {
			cfg.MaxTokens, cfg.HasMaxTokens = n, true
		}
	}
	return cfg, nil
}

// assemblePrompt prepends the gate's prior verdict on repair attempts, VERBATIM
// parity with goose (agent/goose/launch.go — same "<previous verdict>" wrapper
// and json.Marshal of the Feedback map), then the user prompt.
func assemblePrompt(inv agent.AgentInvocation) string {
	prompt := stringOr(inv.With, keyPrompt, "")
	if len(inv.Feedback) > 0 {
		if fb, err := json.Marshal(inv.Feedback); err == nil {
			prompt = fmt.Sprintf("<previous verdict>\n%s\n\n%s", string(fb), prompt)
		}
	}
	// N2: restate the schema in the prompt whenever a typed output is required —
	// ESSENTIAL for structured_output:off (no API JSON signal) and ollama_format (the
	// model never sees Ollama's `format` field); harmless belt-and-suspenders for
	// response_format. Centralized HERE (goose precedent) so the transports don't each
	// inject it (avoids the double-injection the per-transport approach caused).
	if inv.OutputSchema != nil {
		prompt = appendSchemaDirective(prompt, inv.OutputSchema)
	}
	return prompt
}

// appendSchemaDirective restates a required typed-output schema after the given
// text — the always-on portable floor (the assemblePrompt N2 idiom, factored out so
// the react: tool-loop path injects the same directive into its system message).
// A nil schema (or a marshal failure) returns the text unchanged.
func appendSchemaDirective(text string, schema *ir.JSONSchema) string {
	if schema == nil {
		return text
	}
	sb, err := json.Marshal(map[string]any(*schema))
	if err != nil {
		return text
	}
	return text + schemaDirective + string(sb)
}

// schemaDirective nudges the model to make its FINAL message a single conforming JSON
// object (idiom copied from agent/goose's schemaDirective).
const schemaDirective = "\n\nIMPORTANT: your FINAL message must be ONLY a single JSON object conforming exactly to this JSON Schema — no prose before/after, no code fences:\n"

func stringOr(with ir.RawConfig, key, def string) string {
	if v, ok := with[key].(string); ok && v != "" {
		return v
	}
	return def
}

func boolOr(with ir.RawConfig, key string, def bool) bool {
	if v, ok := with[key].(bool); ok {
		return v
	}
	return def
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
