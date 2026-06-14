package awfllm_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A canned OpenAI SSE stream: two content deltas, then a final chunk with
// finish_reason + usage (include_usage), then [DONE].
const openAISSE = `data: {"choices":[{"index":0,"delta":{"content":"{\"ans"}}]}

data: {"choices":[{"index":0,"delta":{"content":"wer\":4}"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}

data: [DONE]

`

func TestStream_OpenAICompat_AccumulatesAndEmits(t *testing.T) {
	var gotBody string
	var gotAuth, gotIdem string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		return sseResponse(openAISSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	var deltas []string
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://api.example.com/v1", APIKey: "sk-test", Model: "gpt-x",
		StructuredOutput: "response_format", IdempotencyKey: "idem-1",
	}
	full, usage, _, finish, err := a.StreamForTest(context.Background(), cfg, "2+2?", &ir.JSONSchema{"type": "object"}, nil,
		func(d string, _ []byte) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if full != `{"answer":4}` {
		t.Errorf("full = %q, want %q", full, `{"answer":4}`)
	}
	if len(deltas) != 2 {
		t.Errorf("emitted %d deltas, want 2 (one per content chunk)", len(deltas))
	}
	if usage.Input != 20 || usage.Output != 5 || usage.CacheRead != 2 {
		t.Errorf("usage = %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotIdem != "idem-1" {
		t.Errorf("Idempotency-Key = %q", gotIdem)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"json_schema"`) || !strings.Contains(gotBody, `"include_usage":true`) {
		t.Errorf("request body missing stream/json_schema/include_usage: %s", gotBody)
	}
	// N1: cfg set no temperature / no token cap → the request MUST omit them
	// (reasoning models reject `temperature` and `max_tokens`).
	if strings.Contains(gotBody, `"temperature"`) || strings.Contains(gotBody, `"max_tokens"`) || strings.Contains(gotBody, `"max_completion_tokens"`) {
		t.Errorf("request body must OMIT temperature/token-cap when unset, got: %s", gotBody)
	}
}

// TestStream_OpenAI_PDFContentPart — Task 7 wire test (OpenAI path). Drives a
// request with one PDF InputFile through the OpenAI transport and asserts the
// captured body carries the `file` content part with the base64 data URI.
func TestStream_OpenAI_PDFContentPart(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(openAISSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://api.example.com/v1", APIKey: "sk-test", Model: "gpt-x",
		StructuredOutput: "off",
	}
	files := []agent.InputFile{{Name: "doc", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}
	_, _, _, _, err := a.StreamWithFilesForTest(context.Background(), cfg, "extract", nil, nil, files,
		func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(gotBody, `"type":"file"`) {
		t.Errorf("body missing file content part: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"file_data":"data:application/pdf;base64,`) {
		t.Errorf("body missing file_data data URI: %s", gotBody)
	}
	// The stable document (file content part) must precede the varying prompt
	// text in the user content array so the document is the cacheable common
	// prefix (OpenAI automatic prompt caching). "extract" is the prompt text.
	if fi, pi := strings.Index(gotBody, `"type":"file"`), strings.Index(gotBody, "extract"); fi < 0 || pi < 0 || fi > pi {
		t.Errorf("file content part (doc) must come BEFORE the prompt text: file@%d prompt@%d\n%s", fi, pi, gotBody)
	}
}

func TestStream_OpenAICompat_400IsPermanent(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"bad model"}}`)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{BaseURL: "https://x/v1", APIKey: "k", Model: "nope", StructuredOutput: "response_format"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "hi", &ir.JSONSchema{"type": "object"}, nil, func(string, []byte) {})
	if err == nil || !awfllm.IsPermanentLLMErrorForTest(err) {
		t.Fatalf("err = %v, want permanent (400 invalid_request_error)", err)
	}
}

// TestStream_OpenAICompat_NoDoubleCallOn429 — AWF owns retries
// (option.WithMaxRetries(0)); openai-go's hidden 429 loop is disabled, so a 429
// must hit the transport exactly ONCE.
func TestStream_OpenAICompat_NoDoubleCallOn429(t *testing.T) {
	var calls int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: 429,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{BaseURL: "https://x/v1", APIKey: "k", Model: "m", StructuredOutput: "off"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "hi", nil, nil, func(string, []byte) {})
	if err == nil {
		t.Fatal("err = nil, want an error for 429")
	}
	if calls != 1 {
		t.Errorf("transport calls = %d, want 1 (AWF owns retries: WithMaxRetries(0))", calls)
	}
}

// TestStream_OpenAICompat_TempAndCap verifies that when HasTemperature and HasMaxTokens
// are set, the wire body contains "temperature" and "max_completion_tokens" (and
// "strict":true for response_format), and does NOT contain the deprecated bare
// "max_tokens" key. This guards the reasoning-model safety invariant: callers must
// never send max_tokens to a completion endpoint.
func TestStream_OpenAICompat_TempAndCap(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(openAISSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	cfg := awfllm.ReqConfigForTest{
		BaseURL:          "https://api.example.com/v1",
		APIKey:           "sk-test",
		Model:            "gpt-x",
		StructuredOutput: "response_format",
		HasTemperature:   true,
		Temperature:      0.2,
		HasMaxTokens:     true,
		MaxTokens:        256,
	}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "hello", &ir.JSONSchema{"type": "object"}, nil, func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(gotBody, `"temperature"`) {
		t.Errorf("body missing temperature: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"max_completion_tokens"`) {
		t.Errorf("body missing max_completion_tokens: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"strict":true`) {
		t.Errorf("body missing strict:true: %s", gotBody)
	}
	// The deprecated "max_tokens" key must not appear. Note: "max_completion_tokens"
	// contains the substring max_tokens, so we check the quoted key form to avoid
	// false positives.
	if strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf(`body must not contain deprecated "max_tokens" key (reasoning models reject it), got: %s`, gotBody)
	}
}

// TestClientFor verifies the tls_insecure composition rules of clientFor:
//   - insecure=false   → returns the injected client unchanged (identity)
//   - insecure=true, fake RoundTripper (non-*http.Transport) → same client (can't clone; left as-is)
//   - insecure=true, default transport (nil Transport in injected client, i.e. New() default) →
//     returns a DIFFERENT client whose Transport has InsecureSkipVerify==true
func TestClientFor(t *testing.T) {
	t.Run("insecure=false returns injected client", func(t *testing.T) {
		injected := &http.Client{}
		a, _ := awfllm.New(awfllm.WithHTTPClient(injected))
		if got := a.ClientForTest(false); got != injected {
			t.Errorf("ClientForTest(false) != injected client; got %p, want %p", got, injected)
		}
	})

	t.Run("insecure=true with fake RoundTripper returns same client", func(t *testing.T) {
		fake := roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, nil })
		injected := &http.Client{Transport: fake}
		a, _ := awfllm.New(awfllm.WithHTTPClient(injected))
		got := a.ClientForTest(true)
		if got != injected {
			t.Errorf("ClientForTest(true) with fake transport must return same client (can't clone non-*http.Transport)")
		}
	})

	t.Run("insecure=true with default transport returns new client with InsecureSkipVerify", func(t *testing.T) {
		// New() with no WithHTTPClient → httpClient has nil Transport (uses http.DefaultTransport).
		a, _ := awfllm.New()
		got := a.ClientForTest(true)
		// Confirm the returned client has InsecureSkipVerify set.
		tr, ok := got.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport type = %T, want *http.Transport", got.Transport)
		}
		if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
			t.Errorf("InsecureSkipVerify = false (or TLSClientConfig nil), want true")
		}
	})
}

// Ollama /api/chat streams NDJSON: per-line message.content deltas, terminal done.
const ollamaNDJSON = `{"message":{"role":"assistant","content":"{\"ans"},"done":false}
{"message":{"role":"assistant","content":"wer\":4}"},"done":false}
{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":11,"eval_count":7}
`

func TestStream_OllamaNative_AccumulatesAndFormat(t *testing.T) {
	var gotURL, gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader(ollamaNDJSON)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		Provider: "ollama",
		BaseURL:  "http://host.docker.internal:11434", APIKey: "ollama", Model: "llama3",
		StructuredOutput: "ollama_format",
	}
	var deltas []string
	full, usage, _, _, err := a.StreamForTest(context.Background(), cfg, "2+2?", &ir.JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}}, nil,
		func(d string, _ []byte) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if full != `{"answer":4}` {
		t.Errorf("full = %q", full)
	}
	if len(deltas) != 2 {
		t.Errorf("emitted %d deltas, want 2", len(deltas))
	}
	if usage.Input != 11 || usage.Output != 7 {
		t.Errorf("usage = %+v, want In:11 Out:7", usage)
	}
	if !strings.HasSuffix(gotURL, "/api/chat") {
		t.Errorf("URL = %q, want .../api/chat (no /v1)", gotURL)
	}
	if !strings.Contains(gotBody, `"format"`) || !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("body missing format/stream: %s", gotBody)
	}
	// The schema rides the `format` field (Ollama-native constraint). Prompt
	// restatement is centralized in assemblePrompt (N2) and verified by
	// TestAssemblePrompt_* — NOT here: StreamForTest receives the prompt verbatim,
	// so this transport-level test must not assert prompt content.
}

// TestStream_ProviderOllama_RoutesToOllamaWire — Fix B: provider is the SOLE
// transport selector. provider:ollama hits the native /api/chat wire EVEN WHEN
// structured_output is response_format (NOT ollama_format) — proving the route is
// driven by cfg.Provider, not by structured_output.
func TestStream_ProviderOllama_RoutesToOllamaWire(t *testing.T) {
	var gotURL string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader(ollamaNDJSON)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		Provider: "ollama",
		BaseURL:  "http://localhost:11434", Model: "llama3",
		StructuredOutput: "response_format", // NOT ollama_format — provider alone must route
	}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "2+2?", &ir.JSONSchema{"type": "object"}, nil, func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.HasSuffix(gotURL, "/api/chat") {
		t.Errorf("URL = %q, want .../api/chat (provider:ollama → native wire)", gotURL)
	}
}

// TestStream_T7_OpenAI_ThreadRendered — T7 wire test (OpenAI path).
// Drives stream with a one-turn thread {User:"u1",Assistant:"a1"}, system prompt
// "SYS", and current prompt "now". Asserts the request messages array is exactly:
//
//	[{role:system,content:SYS},{role:user,content:u1},{role:assistant,content:a1},{role:user,content:now}]
//
// (4 messages, that order) — engine-supplied conversation history rendered correctly.
func TestStream_T7_OpenAI_ThreadRendered(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(openAISSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	thread := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}
	cfg := awfllm.ReqConfigForTest{
		BaseURL:      "https://api.example.com/v1",
		APIKey:       "sk-test",
		Model:        "gpt-x",
		SystemPrompt: "SYS",
		// StructuredOutput intentionally omitted ("") → OpenAI compat, no response_format
	}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "now", nil, thread, func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Parse the messages array from the captured JSON body.
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"` // openai-go may encode content as string or array
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("unmarshal body: %v\nbody: %s", err, gotBody)
	}

	// Helper: extract string content regardless of whether openai-go encodes it as
	// a plain string or a single-item array [{type:text,text:...}].
	contentStr := func(v any) string {
		switch c := v.(type) {
		case string:
			return c
		case []any:
			if len(c) == 1 {
				if m, ok := c[0].(map[string]any); ok {
					if s, ok := m["text"].(string); ok {
						return s
					}
				}
			}
		}
		return ""
	}

	want := []struct{ role, content string }{
		{"system", "SYS"},
		{"user", "u1"},
		{"assistant", "a1"},
		{"user", "now"},
	}
	if len(parsed.Messages) != len(want) {
		t.Fatalf("messages count = %d, want %d; body: %s", len(parsed.Messages), len(want), gotBody)
	}
	for i, w := range want {
		got := parsed.Messages[i]
		gotContent := contentStr(got.Content)
		if got.Role != w.role || gotContent != w.content {
			t.Errorf("messages[%d] = {role:%q, content:%q}, want {role:%q, content:%q}",
				i, got.Role, gotContent, w.role, w.content)
		}
	}
}

// TestStream_T7_Ollama_ThreadRendered — T7 wire test (Ollama native path).
// Same scenario as the OpenAI variant: one prior turn + system + current prompt.
// Asserts the Ollama messages array is:
//
//	[{role:system,content:SYS},{role:user,content:u1},{role:assistant,content:a1},{role:user,content:now}]
func TestStream_T7_Ollama_ThreadRendered(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader(ollamaNDJSON)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	thread := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}
	cfg := awfllm.ReqConfigForTest{
		Provider:         "ollama",
		BaseURL:          "http://localhost:11434",
		Model:            "llama3",
		SystemPrompt:     "SYS",
		StructuredOutput: "ollama_format",
	}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "now", &ir.JSONSchema{"type": "object"}, thread, func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("unmarshal body: %v\nbody: %s", err, gotBody)
	}

	want := []struct{ role, content string }{
		{"system", "SYS"},
		{"user", "u1"},
		{"assistant", "a1"},
		{"user", "now"},
	}
	if len(parsed.Messages) != len(want) {
		t.Fatalf("messages count = %d, want %d; body: %s", len(parsed.Messages), len(want), gotBody)
	}
	for i, w := range want {
		got := parsed.Messages[i]
		if got.Role != w.role || got.Content != w.content {
			t.Errorf("messages[%d] = {role:%q, content:%q}, want {role:%q, content:%q}",
				i, got.Role, got.Content, w.role, w.content)
		}
	}
}

// openAISSEWithModel is a canned OpenAI SSE stream where chunks carry the model
// field (OpenAI puts model on every streamed chunk). The final chunk also includes
// usage with cached_tokens.
const openAISSEWithModel = `data: {"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"content":"hi"}}]}

data: {"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"prompt_tokens_details":{"cached_tokens":200000}}}

data: [DONE]

`

// TestStream_OpenAICompat_WireModelCaptured verifies that the model field carried
// on each SSE chunk is returned as the third return value from StreamForTest.
func TestStream_OpenAICompat_WireModelCaptured(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return sseResponse(openAISSEWithModel), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://api.example.com/v1", APIKey: "sk-test", Model: "gpt-5.3-codex",
		StructuredOutput: "off",
	}
	_, _, gotModel, _, err := a.StreamForTest(context.Background(), cfg, "hello", nil, nil, func(string, []byte) {})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if gotModel != "gpt-5.3-codex" {
		t.Errorf("wire model = %q, want %q", gotModel, "gpt-5.3-codex")
	}
}

// toolCallSSE is a canned OpenAI SSE stream where the assistant emits ONE
// streamed tool_call to "check" with arguments {"iban":"DE89"} (arguments
// fragmented across two deltas, as OpenAI streams them) and finishes with
// finish_reason "tool_calls" + usage (Task 3.4).
const toolCallSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"check","arguments":"{\"iban\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"DE89\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]

`

// finalAnswerSSE is a canned OpenAI SSE stream where the assistant emits a final
// schema-conforming JSON object and finishes with finish_reason "stop" (Task 3.4).
const finalAnswerSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"{\"answer\":"}}]}

data: {"choices":[{"index":0,"delta":{"content":"42}"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]

`

// TestRunOneToolCall_ParsesToolCall: a streamed tool_call response is parsed into
// res.ToolCalls verbatim (ID + Name + Arguments) with the stable Index, and the
// finish_reason is "tool_calls" (Task 3.4).
func TestRunOneToolCall_ParsesToolCall(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return sseResponse(toolCallSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://x/v1", APIKey: "k", Model: "gpt-x", StructuredOutput: "response_format",
	}
	res, err := a.RunOneToolCallForTest(context.Background(), cfg, "react[0].round-1.model",
		[]agent.ReactTurn{{Role: "user", Content: "validate DE89"}},
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		nil)
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", res.FinishReason)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.Name != "check" {
		t.Errorf("name = %q, want check", tc.Name)
	}
	if tc.Arguments != `{"iban":"DE89"}` {
		t.Errorf("arguments = %q, want %q (verbatim)", tc.Arguments, `{"iban":"DE89"}`)
	}
	if tc.ID != "call_abc" {
		t.Errorf("id = %q, want call_abc", tc.ID)
	}
	if tc.Index != 0 {
		t.Errorf("index = %d, want 0", tc.Index)
	}
	// On a tool_calls round, Output stays nil even with a schema.
	if res.Output != nil {
		t.Errorf("Output = %v, want nil on a tool_calls round", res.Output)
	}
}

// TestRunOneToolCall_ResponseFormatOnWire: with structured_output:response_format
// and an output_schema, the request body carries response_format json_schema
// (strict:true) ALONGSIDE the tools array, and the final-answer text is parsed
// into res.Output (#14 — response_format-on-the-wire) (Task 3.4).
func TestRunOneToolCall_ResponseFormatOnWire(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(finalAnswerSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://x/v1", APIKey: "k", Model: "gpt-x", StructuredOutput: "response_format",
	}
	schema := &ir.JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}}
	res, err := a.RunOneToolCallForTest(context.Background(), cfg, "react[0].round-1.model",
		[]agent.ReactTurn{{Role: "user", Content: "what is the answer"}},
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		schema)
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	// #14: response_format on the wire, alongside tools.
	if !strings.Contains(gotBody, `"json_schema"`) || !strings.Contains(gotBody, `"strict":true`) {
		t.Errorf("body missing response_format json_schema/strict:true: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"tools"`) || !strings.Contains(gotBody, `"check"`) {
		t.Errorf("body missing tools array: %s", gotBody)
	}
	// natural-stop + schema → parsed Output.
	if res.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", res.FinishReason)
	}
	if res.Output == nil || res.Output["answer"] == nil {
		t.Fatalf("Output not parsed from final answer: %+v", res.Output)
	}
}

// TestRunOneToolCall_AlwaysOnDirective: even without response_format mode, the
// schema directive is injected into the system message (the portable floor) so the
// request body carries the directive text (Task 3.4).
func TestRunOneToolCall_AlwaysOnDirective(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(finalAnswerSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://x/v1", APIKey: "k", Model: "gpt-x", StructuredOutput: "off",
	}
	schema := &ir.JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}}
	_, err := a.RunOneToolCallForTest(context.Background(), cfg, "react[0].round-1.model",
		[]agent.ReactTurn{{Role: "user", Content: "q"}},
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		schema)
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	// off mode → NO response_format on the wire ...
	if strings.Contains(gotBody, `"response_format"`) || strings.Contains(gotBody, `"json_schema"`) {
		t.Errorf("off mode must NOT send response_format: %s", gotBody)
	}
	// ... but the directive (always-on floor) must be in the system message.
	if !strings.Contains(gotBody, "FINAL message must be ONLY a single JSON object") {
		t.Errorf("schema directive (always-on floor) missing from system message: %s", gotBody)
	}
}

// TestRunOneToolCall_ToolNonStrict: tool FunctionDefinitionParam MUST NOT carry
// strict:true on the wire. The §7 floor on tool input_schema is warn-only
// (AWF2002) so a tool with a non-strict-compatible schema passes awf validate but
// would 400 on the first react round if strict were set. Also, non-OpenAI
// endpoints (vLLM / llama.cpp) 400 on additionalProperties:false/strict itself.
// Response-format strict (mode-guarded by soResponseFormat) is NOT affected.
func TestRunOneToolCall_ToolNonStrict(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(toolCallSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		BaseURL: "https://x/v1", APIKey: "k", Model: "gpt-x", StructuredOutput: "off",
	}
	_, err := a.RunOneToolCallForTest(context.Background(), cfg, "react[0].round-1.model",
		[]agent.ReactTurn{{Role: "user", Content: "q"}},
		// A tool with a non-strict-compatible schema (no additionalProperties:false, no required).
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		nil)
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	// Parse the tools array from the wire body to locate the function definition.
	var parsed struct {
		Tools []struct {
			Function struct {
				Strict *bool `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, gotBody)
	}
	if len(parsed.Tools) == 0 {
		t.Fatalf("tools array empty in wire body: %s", gotBody)
	}
	if fn := parsed.Tools[0].Function; fn.Strict != nil && *fn.Strict {
		t.Errorf("tool function.strict = true on wire, want non-strict (nil/absent); body: %s", gotBody)
	}
}

// TestRunOneToolCall_MessageHistory: assistant turns with stored tool_calls and
// tool-result turns are rendered onto the wire; an assistant turn with NO
// tool_calls must NOT emit an empty tool_calls:[] (a 400) (Task 3.4).
func TestRunOneToolCall_MessageHistory(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return sseResponse(finalAnswerSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{BaseURL: "https://x/v1", APIKey: "k", Model: "gpt-x", StructuredOutput: "off"}
	msgs := []agent.ReactTurn{
		{Role: "user", Content: "u1"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{Index: 0, ID: "call_1", Name: "check", Arguments: `{"x":1}`}}},
		{Role: "tool", ToolCallID: "call_1", Content: "tool-result-here"},
	}
	_, err := a.RunOneToolCallForTest(context.Background(), cfg, "react[0].round-2.model", msgs,
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}}, nil)
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, gotBody)
	}
	// Find the assistant message with the tool_call.
	var foundAsst, foundTool bool
	for _, m := range parsed.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 {
			foundAsst = true
			if m.ToolCalls[0].ID != "call_1" || m.ToolCalls[0].Function.Name != "check" || m.ToolCalls[0].Function.Arguments != `{"x":1}` {
				t.Errorf("assistant tool_call mismatch: %+v", m.ToolCalls[0])
			}
		}
		if m.Role == "tool" {
			foundTool = true
			if m.ToolCallID != "call_1" {
				t.Errorf("tool message tool_call_id = %q, want call_1", m.ToolCallID)
			}
		}
	}
	if !foundAsst {
		t.Errorf("assistant-with-tool_calls message not rendered: %s", gotBody)
	}
	if !foundTool {
		t.Errorf("tool-result message not rendered: %s", gotBody)
	}
	// No empty tool_calls:[] anywhere (would be a 400).
	if strings.Contains(gotBody, `"tool_calls":[]`) {
		t.Errorf("empty tool_calls:[] must be omitted: %s", gotBody)
	}
}

// captureOllamaBody drives a StreamWithFilesForTest request through the Ollama
// transport (StructuredOutput: "ollama_format") with the given files and returns
// the captured request body.
func captureOllamaBody(t *testing.T, files []agent.InputFile) string {
	t.Helper()
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader(ollamaNDJSON)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		Provider:         "ollama",
		BaseURL:          "http://localhost:11434",
		Model:            "llava",
		StructuredOutput: "ollama_format",
	}
	_, _, _, _, err := a.StreamWithFilesForTest(context.Background(), cfg, "describe", nil, nil, files, func(string, []byte) {})
	if err != nil {
		t.Fatalf("StreamWithFilesForTest: %v", err)
	}
	return gotBody
}

// callOllamaWith drives StreamWithFilesForTest through the Ollama transport and
// returns (body, error) — used to test early-return rejection paths.
func callOllamaWith(t *testing.T, files []agent.InputFile) (string, error) {
	t.Helper()
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader(ollamaNDJSON)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		Provider:         "ollama",
		BaseURL:          "http://localhost:11434",
		Model:            "llava",
		StructuredOutput: "ollama_format",
	}
	_, _, _, _, err := a.StreamWithFilesForTest(context.Background(), cfg, "describe", nil, nil, files, func(string, []byte) {})
	return gotBody, err
}

func TestStream_AnthropicDispatched(t *testing.T) {
	// Verify that provider:anthropic routes to callAnthropic (makes an HTTP call).
	var called bool
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return sseAnthropicResponse(anthropicSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "hi", nil, nil, func(string, []byte) {})
	if err != nil {
		t.Fatalf("callAnthropic: %v", err)
	}
	if !called {
		t.Fatal("provider:anthropic must route to callAnthropic (make an HTTP call)")
	}
}

func TestBuildAnthropicBody_DocBreakpointNotPrompt(t *testing.T) {
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", Model: "claude-sonnet-4-6", SystemPrompt: "sys", CacheSystem: true, CacheDocuments: true}
	files := []agent.InputFile{{Name: "d", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}
	thread := []agent.ThreadTurn{{User: "q1", Assistant: "a1"}}
	body, err := awfllm.BuildAnthropicBodyForTest(awfllm.ReqConfigForTest(cfg), "extract", thread, files)
	if err != nil {
		t.Fatal(err)
	}
	// Use json.RawMessage for Content because thread turns carry a string while the
	// current user turn carries an array — the anonymous struct must handle both.
	var req struct {
		System []struct {
			CacheControl map[string]any `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("body shape: %v\n%s", err, raw)
	}
	if req.MaxTokens != 8192 || !req.Stream {
		t.Errorf("max_tokens/stream wrong: %+v", req)
	}
	// cache_system → the system block carries cache_control.
	if len(req.System) != 1 || req.System[0].CacheControl == nil {
		t.Errorf("cache_system must mark the system block: %+v", req.System)
	}
	// Thread order: [user(q1), assistant(a1), current user] — current turn is last.
	if len(req.Messages) != 3 || req.Messages[0].Role != "user" || req.Messages[1].Role != "assistant" || req.Messages[2].Role != "user" {
		t.Fatalf("thread/message order wrong: %+v", req.Messages)
	}
	// Current user content = [document, text] (document FIRST for prefix caching).
	var cur []struct {
		Type         string         `json:"type"`
		CacheControl map[string]any `json:"cache_control"`
	}
	if err := json.Unmarshal(req.Messages[2].Content, &cur); err != nil {
		t.Fatalf("current user content not an array: %v\n%s", err, req.Messages[2].Content)
	}
	if len(cur) != 2 || cur[0].Type != "document" || cur[1].Type != "text" {
		t.Fatalf("current content must be [document, text], got %+v", cur)
	}
	// C1 (load-bearing): cache_control on the DOCUMENT block (the static/varying boundary)…
	if cur[0].CacheControl == nil {
		t.Error("cache_documents must mark the document block")
	}
	// …and NOT on the varying prompt-text block — marking it would change the cached
	// prefix on every repair attempt (verdict prepended), yielding zero cache reads.
	if cur[1].CacheControl != nil {
		t.Error("cache_documents must NOT mark the prompt-text block (it varies per repair)")
	}
}

func TestBuildAnthropicBody_CacheDocumentsNoOpWithoutFiles(t *testing.T) {
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", Model: "claude-haiku-4-5", CacheDocuments: true}
	body, _ := awfllm.BuildAnthropicBodyForTest(awfllm.ReqConfigForTest(cfg), "p", nil, nil)
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "cache_control") {
		t.Errorf("cache_documents with no input files must be a no-op (nothing static to cache): %s", raw)
	}
}

func TestBuildAnthropicBody_ExplicitMaxTokensAndSystemString(t *testing.T) {
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", Model: "claude-haiku-4-5", SystemPrompt: "sys", MaxTokens: 256, HasMaxTokens: true}
	body, _ := awfllm.BuildAnthropicBodyForTest(awfllm.ReqConfigForTest(cfg), "p", nil, nil)
	s := func() string { b, _ := json.Marshal(body); return string(b) }()
	if !strings.Contains(s, `"max_tokens":256`) {
		t.Errorf("explicit max_tokens not honored: %s", s)
	}
	// no cache flags → system is a plain string, no cache_control anywhere.
	if !strings.Contains(s, `"system":"sys"`) || strings.Contains(s, "cache_control") {
		t.Errorf("system should be a plain string with no cache_control: %s", s)
	}
}

func TestBuildAnthropicBody_UnsupportedMIME(t *testing.T) {
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", Model: "claude-haiku-4-5"}
	files := []agent.InputFile{{Name: "x", MIME: "text/plain", Content: []byte("hi")}}
	if _, err := awfllm.BuildAnthropicBodyForTest(awfllm.ReqConfigForTest(cfg), "p", nil, files); err == nil {
		t.Fatal("text/plain must be rejected by the forwardable table")
	}
}

// sseAnthropicResponse returns a 200 text/event-stream response from raw SSE text.
func sseAnthropicResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A canned Anthropic SSE stream: usage on message_start, three text deltas, a
// thinking delta that must be skipped, then message_delta (output + stop) + stop.
const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":20}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"ans"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"wer\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"42}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}

event: message_stop
data: {"type":"message_stop"}

`

func TestCallAnthropic_StreamsAndParses(t *testing.T) {
	var gotBody, gotKey, gotVersion string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		return sseAnthropicResponse(anthropicSSE), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant", Model: "claude-sonnet-4-6"}

	var deltas []string
	full, usage, model, finish, err := a.StreamForTest(context.Background(), cfg, "2+2 as json", nil, nil,
		func(d string, _ []byte) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("callAnthropic: %v", err)
	}
	if full != `{"answer":42}` {
		t.Errorf("full = %q, want %q", full, `{"answer":42}`)
	}
	if len(deltas) != 3 {
		t.Errorf("emitted %d deltas, want 3 (thinking_delta skipped)", len(deltas))
	}
	if usage.Input != 100 || usage.CacheRead != 50 || usage.CacheWrite != 20 || usage.Output != 10 {
		t.Errorf("usage = %+v, want {100 10 50 20} (NO subtract)", usage)
	}
	if finish != "end_turn" {
		t.Errorf("finish = %q, want end_turn", finish)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want cfg.Model", model)
	}
	if !strings.Contains(gotBody, `"stream":true`) || gotKey != "sk-ant" || gotVersion == "" {
		t.Errorf("request wire wrong: key=%q version=%q body=%s", gotKey, gotVersion, gotBody)
	}
}

// C2: a max_tokens truncation must normalize to "length" so launch.go's existing
// truncation path (full=="" || finish=="length") fires uniformly across providers.
func TestCallAnthropic_MaxTokensNormalizedToLength(t *testing.T) {
	const sse = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return sseAnthropicResponse(sse), nil })
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	_, _, _, finish, err := a.StreamForTest(context.Background(), cfg, "x", nil, nil, func(string, []byte) {})
	if err != nil {
		t.Fatal(err)
	}
	if finish != "length" {
		t.Errorf("finish = %q, want length (max_tokens must normalize to the shared truncation sentinel)", finish)
	}
}

// M1: SSE permits a single event's JSON to span multiple data: lines (joined by \n).
// A reframing gateway may do this; the parser must reassemble before unmarshaling.
func TestCallAnthropic_MultiLineDataField(t *testing.T) {
	const sse = "event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\n" +
		"data: \"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return sseAnthropicResponse(sse), nil })
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	full, _, _, _, err := a.StreamForTest(context.Background(), cfg, "x", nil, nil, func(string, []byte) {})
	if err != nil {
		t.Fatal(err)
	}
	if full != "hi" {
		t.Errorf("multi-line data field not reassembled: full = %q, want hi", full)
	}
}

func TestCallAnthropic_HTTP400IsPermanent(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "x", nil, nil, func(string, []byte) {})
	if err == nil || !awfllm.IsPermanentLLMErrorForTest(err) {
		t.Fatalf("400 invalid_request_error must be permanent, got %v", err)
	}
}

func TestCallAnthropic_HTTP429IsRetryable(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 429,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)),
		}, nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "x", nil, nil, func(string, []byte) {})
	if err == nil || awfllm.IsPermanentLLMErrorForTest(err) {
		t.Fatalf("429 must be retryable, got %v", err)
	}
}

func TestCallAnthropic_StreamErrorEvent(t *testing.T) {
	const sse = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return sseAnthropicResponse(sse), nil })
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Model: "claude-sonnet-4-6"}
	_, _, _, _, err := a.StreamForTest(context.Background(), cfg, "x", nil, nil, func(string, []byte) {})
	if err == nil || awfllm.IsPermanentLLMErrorForTest(err) {
		t.Fatalf("overloaded_error stream event must be retryable, got %v", err)
	}
}

// TestStreamOllama_ImagesAndPDFReject — Task 8: images forwarded as BARE base64
// in message.images[]; PDF rejected before any HTTP request.
func TestStreamOllama_ImagesAndPDFReject(t *testing.T) {
	// an image is forwarded as BARE base64 in message.images[]
	body := captureOllamaBody(t, []agent.InputFile{{Name: "s", MIME: "image/png", Content: []byte{1, 2, 3}}})
	if !strings.Contains(body, `"images":["`+base64.StdEncoding.EncodeToString([]byte{1, 2, 3})+`"]`) {
		t.Fatalf("images[] missing/wrong: %s", body)
	}
	// a PDF is rejected BEFORE any request (Ollama is images-only)
	_, err := callOllamaWith(t, []agent.InputFile{{Name: "d", MIME: "application/pdf", Content: []byte("%PDF")}})
	if err == nil {
		t.Fatal("expected PDF rejection on ollama transport")
	}
}
