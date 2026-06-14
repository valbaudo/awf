package awfllm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

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

// newOpenAIClient constructs the openai-go client for this adapter + config.
// option.WithMaxRetries(0) disables the SDK's hidden 429/5xx retry loop — AWF
// owns retries at the engine level.
func (a *Adapter) newOpenAIClient(cfg reqConfig) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(cfg.BaseURL),
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(a.clientFor(cfg.TLSInsecure)),
		option.WithMaxRetries(0),
	)
}

// buildBaseParams returns the ChatCompletionNewParams fields shared by
// streamOpenAI and runOneToolCall: Model, StreamOptions (include_usage),
// optional Temperature + MaxCompletionTokens gates, and the mode-guarded strict
// ResponseFormat block. The caller sets params.Messages (and params.Tools for
// tool-loop rounds) before use. Having a single source means a future param
// added here is automatically shared by both paths.
func buildBaseParams(cfg reqConfig, schema *ir.JSONSchema) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model: cfg.Model,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true), // final chunk carries usage
		},
	}
	if cfg.HasTemperature {
		params.Temperature = openai.Float(cfg.Temperature)
	}
	if cfg.HasMaxTokens {
		// max_completion_tokens — NOT the deprecated max_tokens (reasoning models
		// reject max_tokens). Works for non-reasoning models too on Chat Completions.
		params.MaxCompletionTokens = openai.Int(int64(cfg.MaxTokens))
	}
	if cfg.StructuredOutput == soResponseFormat && schema != nil {
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
	return params
}

// stream issues a STREAMING chat completion, calling emit(delta, rawChunk) per
// content delta, and returns the reassembled text, usage, wire model, finish_reason,
// and a classified error (apiError for HTTP faults). Switches on StructuredOutput:
// ollama_format → the native /api/chat path (Task B6); else → OpenAI-compat.
// thread contains the engine-assembled prior turns (continues: threading) to
// prepend in the message array between the system message and the current prompt.
func (a *Adapter) stream(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	if cfg.Provider == providerGemini {
		return a.callGemini(ctx, cfg, prompt, schema, files, emit)
	}
	if cfg.StructuredOutput == soOllamaFormat {
		return a.streamOllama(ctx, cfg, prompt, schema, thread, files, emit)
	}
	return a.streamOpenAI(ctx, cfg, prompt, schema, thread, files, emit)
}

// streamOpenAI uses openai-go (v3) against any OpenAI-compatible base_url. The
// wire (request body + SSE response) is asserted by transport_test; the openai-go
// Go symbols below are pinned against v3.39.0 (go doc, Task B5 Step 1).
func (a *Adapter) streamOpenAI(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	client := a.newOpenAIClient(cfg)

	// SystemMessage ("system" role) is the right CROSS-BACKEND choice: Ollama/vLLM/
	// llama.cpp expect "system", and OpenAI auto-maps system→developer for reasoning
	// models. Do NOT switch to openai.DeveloperMessage (OpenAI-only; breaks local backends).
	messages := []openai.ChatCompletionMessageParamUnion{}
	if cfg.SystemPrompt != "" {
		messages = append(messages, openai.SystemMessage(cfg.SystemPrompt))
	}
	// Prepend prior turns from the engine-assembled thread (continues: threading).
	// Each turn becomes a user/assistant pair inserted between the system message
	// and the current user message, preserving chronological order.
	for _, t := range thread {
		messages = append(messages, openai.UserMessage(t.User))
		messages = append(messages, openai.AssistantMessage(t.Assistant))
	}
	if len(files) > 0 {
		parts, err := buildOpenAIParts(prompt, files)
		if err != nil {
			return "", usageRec{}, "", "", err
		}
		messages = append(messages, openai.UserMessage(parts))
	} else {
		messages = append(messages, openai.UserMessage(prompt))
	}

	params := buildBaseParams(cfg, schema)
	params.Messages = messages

	opts := []option.RequestOption{}
	if cfg.IdempotencyKey != "" {
		opts = append(opts, option.WithHeader("Idempotency-Key", cfg.IdempotencyKey))
	}

	streamResp := client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer func() { _ = streamResp.Close() }()

	var full strings.Builder
	var usage usageRec
	var model, finish string
	for streamResp.Next() {
		chunk := streamResp.Current()
		raw := []byte(chunk.RawJSON())
		if chunk.Model != "" {
			model = chunk.Model
		}
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
		return full.String(), usage, model, finish, classifyOpenAIErr(err)
	}
	return full.String(), usage, model, finish, nil
}

// runOneToolCall is the tool-aware single model call behind RunToolLoop (P3 A3).
// It is a sibling of streamOpenAI: same OpenAI-compat transport, but it attaches the
// react: step's tools, drives a ChatCompletionAccumulator, and reads the
// AUTHORITATIVE acc.Choices[0].Message after the stream (more reliable than the
// per-chunk JustFinishedToolCall, which is unreliable under parallel tool calls).
// The engine (runReact) owns the message history (msgs); this method executes one
// round and returns the assistant turn + verbatim tool_calls. On a natural stop with
// an output_schema it parses the final text into Output here (the adapter owns
// extractJSONObject) so the engine validates Output without importing agent/awfllm.
func (a *Adapter) runOneToolCall(ctx context.Context, cfg reqConfig, nodePath string, msgs []agent.ReactTurn, tools []agent.ToolDef, schema *ir.JSONSchema) (agent.ToolLoopResult, error) {
	client := a.newOpenAIClient(cfg)

	// Always-on portable floor: inject the schema directive into the system message
	// (the assemblePrompt N2 idiom) so off-OpenAI endpoints that ignore response_format
	// are still steered toward a single conforming JSON object on a natural stop.
	sys := cfg.SystemPrompt
	if schema != nil {
		sys = appendSchemaDirective(sys, schema)
	}

	params := buildBaseParams(cfg, schema)
	params.Messages = buildOpenAIMessages(sys, msgs)
	// ToolChoice left unset → "auto" (the SDK default when Tools is non-empty).
	// Non-strict by default: the §7 floor on tool input_schema is warn-only
	// (AWF2002) so a tool with a non-strict-compatible schema passes awf validate
	// but would 400 on the first react round if strict were set. Also, non-OpenAI
	// endpoints (vLLM / llama.cpp) can 400 on additionalProperties:false/strict.
	for _, td := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        td.Name,
			Description: param.NewOpt(td.Description),
			Parameters:  shared.FunctionParameters(td.InputSchema),
		}))
	}

	opts := []option.RequestOption{}
	if cfg.IdempotencyKey != "" {
		opts = append(opts, option.WithHeader("Idempotency-Key", cfg.IdempotencyKey))
	}

	streamResp := client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer func() { _ = streamResp.Close() }()

	var acc openai.ChatCompletionAccumulator
	var usage usageRec
	for streamResp.Next() {
		chunk := streamResp.Current()
		acc.AddChunk(chunk)
		if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
			usage.Input = int(chunk.Usage.PromptTokens)
			usage.Output = int(chunk.Usage.CompletionTokens)
			usage.CacheRead = int(chunk.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	if err := streamResp.Err(); err != nil {
		return agent.ToolLoopResult{}, classifyOpenAIErr(err)
	}
	if len(acc.Choices) == 0 {
		return agent.ToolLoopResult{}, fmt.Errorf("agent/awfllm: tool-loop response had no choices")
	}

	msg := acc.Choices[0].Message
	ms := a.metricsFrom(usage, cfg.Model)
	res := agent.ToolLoopResult{
		Text:         msg.Content,
		FinishReason: acc.Choices[0].FinishReason, // already a string in openai-go v3.39.0
		Metrics:      &ms,
	}
	// Index is derived from position: the union (ChatCompletionMessageToolCallUnion)
	// carries no Index field (v3.39.0); the streamed order IS the stable J.
	for i, tc := range msg.ToolCalls {
		// Read the promoted flat fields of the union (ChatCompletionMessageToolCallUnion):
		// tc.Function is populated for the function variant by the accumulator. Index is
		// derived from streamed position (the union carries no Index field in v3.39.0).
		res.ToolCalls = append(res.ToolCalls, agent.ToolCall{
			Index:     i,
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	// Natural stop + schema → parse the final text into Output here (the adapter owns
	// extractJSONObject); a parse miss is *agent.ErrUnparseableOutput (retryable). On a
	// tool_calls turn Output stays nil (the model has not produced the final answer).
	if res.FinishReason != "tool_calls" && schema != nil {
		obj, perr := extractJSONObject(msg.Content)
		if perr != nil {
			return res, &agent.ErrUnparseableOutput{NodePath: nodePath}
		}
		res.Output = obj
	}
	return res, nil
}

// buildOpenAIMessages maps the engine-owned []agent.ReactTurn onto the openai-go
// message param union: user→UserMessage, tool→ToolMessage(content, ToolCallID),
// assistant→an assistant param carrying any stored tool_calls. An assistant turn
// with NO tool_calls is a plain AssistantMessage — an empty tool_calls:[] on the
// wire is a 400, so it is omitted entirely. systemPrompt (already carrying the
// always-on schema directive, if any) becomes the leading system message.
func buildOpenAIMessages(systemPrompt string, turns []agent.ReactTurn) []openai.ChatCompletionMessageParamUnion {
	messages := []openai.ChatCompletionMessageParamUnion{}
	if systemPrompt != "" {
		messages = append(messages, openai.SystemMessage(systemPrompt))
	}
	for _, t := range turns {
		switch t.Role {
		case "user":
			messages = append(messages, openai.UserMessage(t.Content))
		case "tool":
			messages = append(messages, openai.ToolMessage(t.Content, t.ToolCallID))
		case "assistant":
			if len(t.ToolCalls) == 0 {
				// Plain assistant text turn — OMIT tool_calls (an empty [] is a 400).
				messages = append(messages, openai.AssistantMessage(t.Content))
				continue
			}
			asst := openai.ChatCompletionAssistantMessageParam{}
			if t.Content != "" {
				asst.Content = openai.ChatCompletionAssistantMessageParamContentUnion{OfString: param.NewOpt(t.Content)}
			}
			for _, tc := range t.ToolCalls {
				asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: tc.Arguments,
						},
					},
				})
			}
			messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		}
	}
	return messages
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
func (a *Adapter) streamOllama(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/api/chat"

	type msg struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images,omitempty"` // bare standard-base64, NOT data URIs (Ollama /api/chat format)
	}
	messages := []msg{}
	if cfg.SystemPrompt != "" {
		messages = append(messages, msg{Role: "system", Content: cfg.SystemPrompt})
	}
	// Prepend prior turns from the engine-assembled thread (continues: threading).
	// Each turn becomes a user/assistant pair inserted before the current user message.
	for _, t := range thread {
		messages = append(messages, msg{Role: "user", Content: t.User})
		messages = append(messages, msg{Role: "assistant", Content: t.Assistant})
	}
	userMsg := msg{Role: "user", Content: prompt}
	for _, f := range files {
		if !strings.HasPrefix(f.MIME, "image/") {
			return "", usageRec{}, "", "", &agent.ErrInvalidConfig{
				Ref:    AdapterRef,
				Key:    "input_files",
				Reason: "ollama transport supports images only, not " + f.MIME + "; rasterize the PDF to images first",
			}
		}
		// Bare standard-base64 — NOT a data URI. Ollama /api/chat images[] is
		// the OPPOSITE of the OpenAI path (which uses data:...;base64, URIs).
		userMsg.Images = append(userMsg.Images, base64.StdEncoding.EncodeToString(f.Content))
	}
	messages = append(messages, userMsg)

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
		return "", usageRec{}, "", "", err
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
		return "", usageRec{}, "", "", err // transport → retryable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", usageRec{}, "", "", &apiError{Status: resp.StatusCode, Type: ollamaErrType(tail), Body: string(tail)}
	}

	var full strings.Builder
	var usage usageRec
	var model, finish string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev struct {
			Model   string `json:"model"`
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
		if ev.Model != "" {
			model = ev.Model
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
		return full.String(), usage, model, finish, err // mid-stream drop → retryable
	}
	return full.String(), usage, model, finish, nil
}

// ollamaErrType extracts an error type hint from an Ollama error body
// (best-effort). Returns errTypeInvalidRequest only if the body's `error`
// field contains "invalid"; else "ollama_error".
func ollamaErrType(body []byte) string {
	var probe struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && strings.Contains(probe.Error, "invalid") {
		return errTypeInvalidRequest
	}
	return "ollama_error"
}

// callGemini hits Google's NATIVE Gemini :generateContent (NOT the OpenAI-compat
// path). v1 is NON-STREAMING: one POST, parse the single JSON response, emit the
// full text once. inlineData carries BARE standard-base64 (no data: URI prefix)
// under mimeType — the same wire as Ollama images[], the OPPOSITE of the OpenAI
// content-part path's data: URIs.
//
// Structured output: an output_schema sets generationConfig.responseMimeType to
// application/json AND _responseJsonSchema to the schema verbatim. AWF schemas use
// the supported JSON-Schema subset; Gemini IGNORES unsupported keywords (does not
// 400). Auth is the x-goog-api-key header; Gemini documents no Idempotency-Key, so
// none is sent. Cost is left UNREPORTED automatically — pricing.Derive returns
// ok=false for any model absent from rates.json, and metricsFrom skips cost on a
// miss (no special-casing here).
func (a *Adapter) callGemini(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1beta/models/" + cfg.Model + ":generateContent"

	parts := []map[string]any{{"text": prompt}}
	for _, f := range files {
		parts = append(parts, map[string]any{"inlineData": map[string]any{
			"mimeType": f.MIME,
			"data":     base64.StdEncoding.EncodeToString(f.Content), // bare base64, no data: prefix
		}})
	}
	body := map[string]any{"contents": []map[string]any{{"role": "user", "parts": parts}}}
	if cfg.SystemPrompt != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": cfg.SystemPrompt}}}
	}
	gc := map[string]any{}
	if cfg.HasTemperature {
		gc["temperature"] = cfg.Temperature
	}
	if cfg.HasMaxTokens {
		gc["maxOutputTokens"] = cfg.MaxTokens
	}
	if schema != nil {
		gc["responseMimeType"] = "application/json"
		gc["_responseJsonSchema"] = map[string]any(*schema)
	}
	if len(gc) > 0 {
		body["generationConfig"] = gc
	}
	reqBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return "", usageRec{}, "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.APIKey)
	// No Idempotency-Key: Gemini's REST API documents none.

	resp, err := a.clientFor(cfg.TLSInsecure).Do(req)
	if err != nil {
		return "", usageRec{}, "", "", err // transport → retryable
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return "", usageRec{}, "", "", &apiError{Status: resp.StatusCode, Type: geminiErrType(respBytes), Body: string(respBytes)}
	}

	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBytes, &gr); err != nil {
		return "", usageRec{}, "", "", err
	}
	var full strings.Builder
	var finish string
	if len(gr.Candidates) > 0 {
		for _, p := range gr.Candidates[0].Content.Parts {
			full.WriteString(p.Text)
		}
		finish = gr.Candidates[0].FinishReason
	}
	usage := usageRec{
		Input:     gr.UsageMetadata.PromptTokenCount,
		Output:    gr.UsageMetadata.CandidatesTokenCount,
		CacheRead: gr.UsageMetadata.CachedContentTokenCount,
	}
	text := full.String()
	emit(text, respBytes) // single emit: non-streaming v1
	return text, usage, cfg.Model, finish, nil
}

// geminiErrType maps a Gemini error body to a coarse type so a 400 INVALID_ARGUMENT
// classifies permanent (like OpenAI's invalid_request_error); everything else stays
// retryable.
func geminiErrType(body []byte) string {
	var probe struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error.Status == "INVALID_ARGUMENT" {
		return errTypeInvalidRequest
	}
	return "gemini_error"
}
