package awfllm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/valbaudo/awf/ir"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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

// clientFor returns the HTTP client for a call. Normally the injected client.
// When tls_insecure is set, it derives a client that skips TLS verification — for
// reaching self-signed / internal LLM endpoints (a legitimate pentest need). It
// COMPOSES with the injected transport so tests (which inject a fake
// http.RoundTripper) are unaffected: a non-*http.Transport (the fake) is returned
// unchanged; only a real *http.Transport (or the default) is cloned with
// InsecureSkipVerify. Proxies need no knob — Go's default transport already honors
// HTTP(S)_PROXY via http.ProxyFromEnvironment.
func (a *Adapter) clientFor(insecure bool) *http.Client {
	if !insecure {
		return a.httpClient
	}
	base, ok := a.httpClient.Transport.(*http.Transport)
	if !ok {
		if a.httpClient.Transport != nil {
			return a.httpClient // custom non-*http.Transport (e.g. test RoundTripper): can't toggle TLS; leave as-is
		}
		base = http.DefaultTransport.(*http.Transport)
	}
	tr := base.Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{} //nolint:gosec // G402 set intentionally below
	}
	tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // G402: opt-in tls_insecure for internal/self-signed endpoints
	c := *a.httpClient
	c.Transport = tr
	return &c
}

// stream issues a STREAMING chat completion, calling emit(delta, rawChunk) per
// content delta, and returns the reassembled text, usage, finish_reason, and a
// classified error (apiError for HTTP faults). Switches on StructuredOutput:
// ollama_format → the native /api/chat path (Task B6); else → OpenAI-compat.
func (a *Adapter) stream(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, emit func(delta string, raw []byte)) (string, usageRec, string, error) {
	if cfg.StructuredOutput == "ollama_format" {
		return a.streamOllama(ctx, cfg, prompt, schema, emit)
	}
	return a.streamOpenAI(ctx, cfg, prompt, schema, emit)
}

// streamOpenAI uses openai-go (v3) against any OpenAI-compatible base_url. The
// wire (request body + SSE response) is asserted by transport_test; the openai-go
// Go symbols below are pinned against v3.39.0 (go doc, Task B5 Step 1).
func (a *Adapter) streamOpenAI(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, emit func(delta string, raw []byte)) (string, usageRec, string, error) {
	client := openai.NewClient(
		option.WithBaseURL(cfg.BaseURL),
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(a.clientFor(cfg.TLSInsecure)),
		option.WithMaxRetries(0), // AWF owns retries — disable openai-go's hidden 429/5xx loop
	)

	// SystemMessage ("system" role) is the right CROSS-BACKEND choice: Ollama/vLLM/
	// llama.cpp expect "system", and OpenAI auto-maps system→developer for reasoning
	// models. Do NOT switch to openai.DeveloperMessage (OpenAI-only; breaks local backends).
	messages := []openai.ChatCompletionMessageParamUnion{}
	if cfg.SystemPrompt != "" {
		messages = append(messages, openai.SystemMessage(cfg.SystemPrompt))
	}
	messages = append(messages, openai.UserMessage(prompt))

	params := openai.ChatCompletionNewParams{
		Model:    cfg.Model,
		Messages: messages,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true), // final chunk carries usage
		},
	}
	if cfg.HasTemperature {
		params.Temperature = openai.Float(cfg.Temperature)
	}
	if cfg.HasMaxTokens {
		// max_completion_tokens — NOT the deprecated max_tokens (reasoning models reject
		// max_tokens). Works for non-reasoning models too on Chat Completions.
		params.MaxCompletionTokens = openai.Int(int64(cfg.MaxTokens))
	}
	if cfg.StructuredOutput == "response_format" && schema != nil {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &openai.ResponseFormatJSONSchemaParam{
				JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "output",
					Schema: map[string]any(*schema),
					Strict: openai.Bool(true),
				},
			},
		}
	}

	opts := []option.RequestOption{}
	if cfg.IdempotencyKey != "" {
		opts = append(opts, option.WithHeader("Idempotency-Key", cfg.IdempotencyKey))
	}

	streamResp := client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer func() { _ = streamResp.Close() }()

	var full strings.Builder
	var usage usageRec
	var finish string
	for streamResp.Next() {
		chunk := streamResp.Current()
		raw := []byte(chunk.RawJSON())
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				full.WriteString(delta)
				emit(delta, append([]byte(nil), raw...))
			}
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				finish = fr
			}
		}
		if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
			usage.Input = int(chunk.Usage.PromptTokens)
			usage.Output = int(chunk.Usage.CompletionTokens)
			usage.CacheRead = int(chunk.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	if err := streamResp.Err(); err != nil {
		return full.String(), usage, finish, classifyOpenAIErr(err)
	}
	return full.String(), usage, finish, nil
}

// classifyOpenAIErr maps an *openai.Error to our wire-shaped apiError so
// isPermanentLLMError can decide permanent (400 invalid_request_error) vs
// retryable uniformly. Non-API (transport/decode) errors pass through unchanged
// (→ retryable default).
func classifyOpenAIErr(err error) error {
	var oe *openai.Error
	if errors.As(err, &oe) {
		return &apiError{Status: oe.StatusCode, Type: oe.Type, Body: oe.Error()}
	}
	return err
}

// streamOllama hits Ollama's NATIVE /api/chat (NOT the OpenAI-compat /v1 path,
// which ignores json_schema). The schema rides the `format` field. Streams
// NDJSON: one object per line, terminal {done:true}.
//
// Token cap → options.num_predict (Ollama's field; NOT max_tokens).
// Temperature → options.temperature. The options map is only attached when
// at least one option is set.
//
// Do NOT prepend the schema to the prompt here — schema restatement is
// centralized in assemblePrompt (Task B7) for ALL modes. Send prompt as-is.
func (a *Adapter) streamOllama(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, emit func(delta string, raw []byte)) (string, usageRec, string, error) {
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/api/chat"

	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	messages := []msg{}
	if cfg.SystemPrompt != "" {
		messages = append(messages, msg{Role: "system", Content: cfg.SystemPrompt})
	}
	messages = append(messages, msg{Role: "user", Content: prompt})

	body := map[string]any{"model": cfg.Model, "messages": messages, "stream": true}
	if schema != nil {
		body["format"] = map[string]any(*schema)
	}
	opts := map[string]any{}
	if cfg.HasTemperature {
		opts["temperature"] = cfg.Temperature
	}
	if cfg.HasMaxTokens {
		opts["num_predict"] = cfg.MaxTokens // Ollama's token-cap field (NOT max_tokens / max_completion_tokens)
	}
	if len(opts) > 0 {
		body["options"] = opts
	}
	reqBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return "", usageRec{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	if cfg.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", cfg.IdempotencyKey)
	}

	resp, err := a.clientFor(cfg.TLSInsecure).Do(req)
	if err != nil {
		return "", usageRec{}, "", err // transport → retryable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", usageRec{}, "", &apiError{Status: resp.StatusCode, Type: ollamaErrType(tail), Body: string(tail)}
	}

	var full strings.Builder
	var usage usageRec
	var finish string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done            bool   `json:"done"`
			DoneReason      string `json:"done_reason"`
			PromptEvalCount int    `json:"prompt_eval_count"`
			EvalCount       int    `json:"eval_count"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // tolerate a stray non-JSON line
		}
		if ev.Message.Content != "" {
			full.WriteString(ev.Message.Content)
			emit(ev.Message.Content, append([]byte(nil), line...))
		}
		if ev.Done {
			usage.Input = ev.PromptEvalCount
			usage.Output = ev.EvalCount
			finish = ev.DoneReason
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), usage, finish, err // mid-stream drop → retryable
	}
	return full.String(), usage, finish, nil
}

// ollamaErrType extracts an error type hint from an Ollama error body
// (best-effort). Returns "invalid_request_error" only if the body's `error`
// field contains "invalid"; else "ollama_error".
func ollamaErrType(body []byte) string {
	var probe struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && strings.Contains(probe.Error, "invalid") {
		return "invalid_request_error"
	}
	return "ollama_error"
}
