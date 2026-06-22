package awfllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// buildAnthropicBody assembles the /v1/messages request body: optional cacheable
// system block, prior thread turns as user/assistant pairs, then the current user
// turn with document blocks FIRST and the prompt text LAST (document-first for
// prefix caching). cache_system marks the system block; cache_documents marks the
// last DOCUMENT block (NOT the prompt) unless cache_context marks the later context
// block. Files are gated by the shared forwardable() capability table.
func buildAnthropicBody(cfg reqConfig, prompt string, thread []agent.ThreadTurn, contextEvidence []agent.ThreadTurn, files []agent.InputFile) (map[string]any, error) {
	// Current user content: documents first, prompt last.
	content := make([]map[string]any, 0, len(files)+2)
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
	if cfg.CacheDocuments && !cfg.CacheContext && len(files) > 0 {
		content[len(files)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	contextText := renderContextEvidence(contextEvidence)
	if contextText != "" {
		block := map[string]any{"type": "text", "text": contextText}
		if cfg.CacheContext {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		content = append(content, block)
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

// anthropicEvent is the subset of an SSE `data:` payload we read. We switch on the
// JSON `type` field (robust against a missing `event:` line — spec O10).
type anthropicEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// callAnthropic hits the native Anthropic Messages API (POST /v1/messages) with
// SSE streaming. Parallel to callGemini/streamOllama: hand-rolled net/http, one
// AgentEvent per text_delta, full text reassembled for the layer-2 parse.
func (a *Adapter) callAnthropic(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, contextEvidence []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	body, err := buildAnthropicBody(cfg, prompt, thread, contextEvidence, files)
	if err != nil {
		return "", usageRec{}, "", "", err // *ErrInvalidConfig (unsupported MIME) → permanent
	}
	reqBytes, _ := json.Marshal(body)

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return "", usageRec{}, "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.clientFor(cfg.TLSInsecure).Do(req)
	if err != nil {
		return "", usageRec{}, "", "", err // transport → retryable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ra, sr := parseRetrySignals(resp.Header, time.Now())
		return "", usageRec{}, "", "", &apiError{Status: resp.StatusCode, Type: anthropicErrType(tail), Body: string(tail), RetryAfter: ra, ShouldRetry: sr}
	}

	var full strings.Builder
	var usage usageRec
	var finish string
	var streamErr error
	var dataBuf []byte // accumulates one event's (possibly multi-line) data field

	// dispatch folds one complete SSE event payload into the result. We switch on the
	// JSON `type` (robust to a missing/extra `event:` line).
	dispatch := func(payload []byte) {
		var ev anthropicEvent
		if json.Unmarshal(payload, &ev) != nil {
			return // tolerate a non-JSON event
		}
		switch ev.Type {
		case "message_start":
			usage.Input = ev.Message.Usage.InputTokens
			usage.CacheRead = ev.Message.Usage.CacheReadInputTokens
			usage.CacheWrite = ev.Message.Usage.CacheCreationInputTokens
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				full.WriteString(ev.Delta.Text)
				emit(ev.Delta.Text, append([]byte(nil), payload...))
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish = ev.Delta.StopReason
				if finish == "max_tokens" {
					finish = "length" // shared truncation sentinel launch.go checks (OpenAI uses "length")
				}
			}
			if ev.Usage.OutputTokens != 0 {
				usage.Output = ev.Usage.OutputTokens
			}
		case "error":
			streamErr = anthropicStreamErr(ev.Error.Type, ev.Error.Message)
		}
	}

	// Proper SSE framing: a blank line ends an event; consecutive `data:` lines for
	// one event are joined with "\n" before parsing (robust to reframing gateways,
	// not just Anthropic's single-line wire). event:/id:/retry:/comment lines ignored.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() && streamErr == nil {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 { // event boundary
			if len(dataBuf) > 0 {
				dispatch(dataBuf)
				dataBuf = dataBuf[:0]
			}
			continue
		}
		if seg, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			seg = bytes.TrimPrefix(seg, []byte(" ")) // SSE strips one leading space
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, seg...)
		}
	}
	if streamErr == nil && len(dataBuf) > 0 {
		dispatch(dataBuf) // final event if the stream did not end with a blank line
	}
	if err := scanner.Err(); err != nil {
		return full.String(), usage, cfg.Model, finish, err // mid-stream drop → retryable
	}
	if streamErr != nil {
		return full.String(), usage, cfg.Model, finish, streamErr
	}
	return full.String(), usage, cfg.Model, finish, nil
}

// anthropicErrType maps an HTTP error body to a coarse type so a 400
// invalid_request_error classifies permanent (like OpenAI/Gemini); else retryable.
func anthropicErrType(body []byte) string {
	var probe struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error.Type == errTypeInvalidRequest {
		return errTypeInvalidRequest
	}
	return "anthropic_error"
}

// anthropicStreamErr maps a mid-stream SSE error event to an *apiError. An
// invalid_request_error gets Status 400 so isPermanentLLMError classifies it
// permanent; everything else gets a 503-equivalent → retryable.
func anthropicStreamErr(typ, msg string) error {
	status := 503
	if typ == errTypeInvalidRequest {
		status = 400
	}
	return &apiError{Status: status, Type: typ, Body: msg}
}
