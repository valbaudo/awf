package awfllm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

const eventsBuffer = 16

// Launch honors the γ contract: returns immediately with both channels open. A
// single goroutine drives the stream, emitting one AgentEvent per delta
// (DisplayAssistantDelta — renders char-by-char), then a DisplayFinal, then the
// outcome. The container.Handle is ignored (this adapter calls HTTP directly).
func (a *Adapter) Launch(ctx context.Context, _ container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	cfg, err := a.buildReqConfig(inv)
	if err != nil {
		return nil, nil, err // pre-launch failure: both channels nil
	}
	prompt := assemblePrompt(inv)

	events := make(chan agent.AgentEvent, eventsBuffer)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	go func() {
		// LIFO: events closes before outcomeCh (documented order).
		defer close(outcomeCh)
		defer close(events)

		emit := func(delta string, raw []byte) {
			ev := agent.AgentEvent{
				Kind:    "delta",
				Stream:  "stdout",
				Payload: raw, // already a fresh copy from transport
				// RAW delta text — NOT agent.Elide (Elide clips a >512B line with an
				// ellipsis marker, which would corrupt the live stream). DisplayAssistantDelta
				// (Task A7) makes the renderer write it WITHOUT a trailing newline → the
				// deltas concatenate character-by-character.
				Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: delta},
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		full, usage, finish, serr := a.stream(ctx, cfg, prompt, inv.OutputSchema, emit)
		if serr != nil {
			outcomeCh <- agent.AgentOutcome{Err: classifyLaunchErr(serr)}
			return
		}

		// Terminal display.
		select {
		case events <- agent.AgentEvent{Kind: "done", Stream: "stdout", Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: tokenSummary(usage)}}:
		case <-ctx.Done():
		}

		// Empty output, refusal, or truncation → retryable unparseable.
		if full == "" || finish == "length" {
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
			return
		}
		res, berr := buildResult(full, usage, inv)
		if berr != nil {
			outcomeCh <- agent.AgentOutcome{Err: berr} // ErrUnparseableOutput (retryable)
			return
		}
		outcomeCh <- agent.AgentOutcome{Result: res}
	}()

	return events, outcomeCh, nil
}

// classifyLaunchErr maps a transport/stream error to a mechanical class:
// permanent (*agent.ErrInvalidConfig) iff a 400 invalid_request_error; else
// retryable (*agent.ErrAgentLaunch).
func classifyLaunchErr(err error) error {
	if isPermanentLLMError(err) {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "", Reason: err.Error()}
	}
	return &agent.ErrAgentLaunch{Cause: err} // agent.ErrAgentLaunch is {Cause error} ONLY — no Ref field (agent/errors.go:62)
}

// buildReqConfig translates validated `with:` + the resolved env into a reqConfig.
func (a *Adapter) buildReqConfig(inv agent.AgentInvocation) (reqConfig, error) {
	with := inv.With
	cfg := reqConfig{
		Model:            stringOr(with, keyModel, ""),
		BaseURL:          stringOr(with, keyBaseURL, "https://api.openai.com/v1"), // default OpenAI; documented footgun — see B9 docs
		SystemPrompt:     stringOr(with, keySystemPrompt, ""),
		StructuredOutput: stringOr(with, keyStructuredOutput, "response_format"),
		IdempotencyKey:   inv.IdempotencyKey,
		TLSInsecure:      boolOr(with, keyTLSInsecure, false),
	}
	keyName := stringOr(with, keyAPIKeyEnv, defaultAPIKeyEnv)
	key, ok := a.env[keyName]
	if !ok {
		// Defensive (ValidateConfig already checked). Return the PERMANENT-classified
		// type: ErrMissingAPIKey is adapter-local and would hit the retryable default
		// in classifyAgentLaunchErr, whereas *agent.ErrInvalidConfig → permanent.
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
		if sb, err := json.Marshal(map[string]any(*inv.OutputSchema)); err == nil {
			prompt = prompt + schemaDirective + string(sb)
		}
	}
	return prompt
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

var _ agent.Adapter = (*Adapter)(nil)
