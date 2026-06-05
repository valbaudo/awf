package awfllm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
	full, usage, finish, err := a.StreamForTest(context.Background(), cfg, "2+2?", &ir.JSONSchema{"type": "object"},
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
	_, _, _, err := a.StreamForTest(context.Background(), cfg, "hi", &ir.JSONSchema{"type": "object"}, func(string, []byte) {})
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
	_, _, _, err := a.StreamForTest(context.Background(), cfg, "hi", nil, func(string, []byte) {})
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
	_, _, _, err := a.StreamForTest(context.Background(), cfg, "hello", &ir.JSONSchema{"type": "object"}, func(string, []byte) {})
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
		BaseURL: "http://host.docker.internal:11434", APIKey: "ollama", Model: "llama3",
		StructuredOutput: "ollama_format",
	}
	var deltas []string
	full, usage, _, err := a.StreamForTest(context.Background(), cfg, "2+2?", &ir.JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}},
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
