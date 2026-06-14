package awfllm

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// buildAnthropicBody assembles the /v1/messages request body: optional cacheable
// system block, prior thread turns as user/assistant pairs, then the current user
// turn with document blocks FIRST and the prompt text LAST (document-first for
// prefix caching). cache_system marks the system block; cache_documents marks the
// last DOCUMENT block (NOT the prompt). Files are gated by the shared forwardable()
// capability table.
func buildAnthropicBody(cfg reqConfig, prompt string, thread []agent.ThreadTurn, files []agent.InputFile) (map[string]any, error) {
	// Current user content: documents first, prompt last.
	content := make([]map[string]any, 0, len(files)+1)
	for _, f := range files {
		m, ok := forwardable(providerAnthropic, f.MIME)
		if !ok {
			return nil, unsupportedMIMEErr(f.MIME, "")
		}
		b64 := base64.StdEncoding.EncodeToString(f.Content)
		switch m {
		case modalityImage:
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": f.MIME, "data": b64}})
		case modalityDocument:
			content = append(content, map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": b64}})
		}
	}
	// cache_documents marks the LAST FILE block — the boundary between the static
	// document(s) and the varying prompt — so [tools→system→…→documents] is the
	// cached prefix and the (per-repair-varying) prompt is the uncached suffix.
	// Marking the prompt block instead would change the cached prefix on every
	// repair attempt and never yield a cache read (Anthropic "common mistake").
	// No-op without files: nothing static to cache here (use system_prompt +
	// cache_system for large stable instructions).
	if cfg.CacheDocuments && len(files) > 0 {
		content[len(files)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	// Messages: prior thread turns (user/assistant pairs) then the current user turn.
	messages := make([]map[string]any, 0, len(thread)*2+1)
	for _, t := range thread {
		messages = append(messages,
			map[string]any{"role": "user", "content": t.User},
			map[string]any{"role": "assistant", "content": t.Assistant},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": content})

	maxTok := anthropicDefaultMaxTokens
	if cfg.HasMaxTokens {
		maxTok = cfg.MaxTokens
	}
	body := map[string]any{
		"model":      cfg.Model,
		"max_tokens": maxTok,
		"stream":     true,
		"messages":   messages,
	}
	if cfg.SystemPrompt != "" {
		if cfg.CacheSystem {
			body["system"] = []map[string]any{{"type": "text", "text": cfg.SystemPrompt, "cache_control": map[string]any{"type": "ephemeral"}}}
		} else {
			body["system"] = cfg.SystemPrompt
		}
	}
	if cfg.HasTemperature {
		body["temperature"] = cfg.Temperature
	}
	return body, nil
}

// callAnthropic is the native Anthropic Messages-API transport (POST /v1/messages,
// SSE streaming). Stub until Task 8.
func (a *Adapter) callAnthropic(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	return "", usageRec{}, "", "", fmt.Errorf("agent/awfllm: callAnthropic not implemented")
}
