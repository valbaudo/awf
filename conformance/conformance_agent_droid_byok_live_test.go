//go:build integ && live

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

// byokTestKeyEnv is the host env var name the test forwards to droid. Its VALUE
// (the placeholder-expanded secret) must reach the mock gateway as a Bearer
// header WITHOUT ever appearing in the assembled command string — that round
// trip is the whole point of the test.
const byokTestKeyEnv = "AWF_BYOK_TEST_KEY"

// byokTestKeyValue is the fake "secret" the mock asserts it received. Any value
// works; a sk-* shape just mirrors a real OpenAI-style key.
const byokTestKeyValue = "sk-live-smoke-12345"

// byokLaunchTimeout bounds a real droid cold-start (binary init + BYOK dial) so a
// protocol mismatch fails clearly instead of hanging.
const byokLaunchTimeout = 120 * time.Second

// recordedRequest captures the gateway-relevant facts of one inbound HTTP
// request to the mock chat-completions endpoint.
type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Model  string
}

// byokMockHandler returns an http.Handler that records each inbound chat-
// completions request and replies with a minimal OpenAI response. It supports
// both stream:true (SSE chunks ending with data: [DONE]) — which is what droid
// sends — and a non-streaming JSON fallback. Recorded requests are appended
// under mu; the caller inspects them after Launch drains.
func byokMockHandler(t *testing.T, mu *sync.Mutex, recorded *[]recordedRequest) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		// Pull the model field out of the request body (best-effort: a
		// non-chat-completions probe, e.g. a model-list GET, has no body/model).
		var parsed struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &parsed)

		mu.Lock()
		*recorded = append(*recorded, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Model:  parsed.Model,
		})
		mu.Unlock()

		// Only the chat-completions POST gets a completion reply. Anything else
		// (a capability/model probe) gets a benign 200 so droid doesn't bail on a
		// transport error before it ever reaches the completion call.
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}

		if parsed.Stream {
			writeSSECompletion(w, parsed.Model)
			return
		}
		writeJSONCompletion(w, parsed.Model)
	})
}

// writeSSECompletion emits a minimal streamed chat.completion: one content
// delta then a finish_reason:stop chunk, terminated by the data:[DONE] sentinel.
func writeSSECompletion(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	chunk := func(delta map[string]any, finish any) {
		payload := map[string]any{
			"id":      "chatcmpl-awfsmoke",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	chunk(map[string]any{"role": "assistant"}, nil)
	chunk(map[string]any{"content": "hi"}, nil)
	chunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeJSONCompletion emits a minimal non-streamed chat.completion (the
// fallback path if droid ever sends stream:false).
func writeJSONCompletion(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-awfsmoke",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "hi"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	b, _ := json.Marshal(resp)
	_, _ = w.Write(b)
}

// runDroidBYOK drives one real `droid exec` against the given gateway base URL
// (server URL + "/v1") with the BYOK with-config, draining events + outcome.
// It returns the outcome and any Launch error. tlsInsecure flips the
// tls_insecure with-key (→ NODE_TLS_REJECT_UNAUTHORIZED=0 in the exec env).
func runDroidBYOK(t *testing.T, baseURL, model string, tlsInsecure bool) (agent.AgentOutcome, error) {
	t.Helper()

	nb, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	// Forward the test key var into droid's exec env via the allowlist. droid
	// expands the ${AWF_BYOK_TEST_KEY} placeholder in the customModels settings
	// file from its own process env — proving the key never touches the command.
	ad, err := droid.New(
		droid.WithEnv(envFromHost([]string{byokTestKeyEnv})),
		droid.WithBackend(nb),
	)
	if err != nil {
		t.Fatalf("droid.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), byokLaunchTimeout)
	defer cancel()

	h, err := nb.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Backend.Create: %v", err)
	}
	t.Cleanup(func() { _ = nb.Destroy(context.Background(), h) })

	with := ir.RawConfig{
		"base_url":    baseURL,
		"api_key_env": byokTestKeyEnv,
		"provider":    "generic-chat-completion-api",
		"model":       model,
		"prompt":      "say hi",
		// keep droid from touching the filesystem / asking for permission
		"autonomy": "read-only",
	}
	if tlsInsecure {
		with["tls_insecure"] = true
	}

	inv := agent.AgentInvocation{
		NodePath: "/test/droid-byok",
		Uses:     ad.Ref(),
		With:     with,
	}

	events, outcomeCh, err := ad.Launch(ctx, h, inv)
	if err != nil {
		return agent.AgentOutcome{}, err
	}
	for range events {
	}
	oc, ok := <-outcomeCh
	if !ok {
		t.Fatal("outcome channel closed without emitting")
	}
	if _, more := <-outcomeCh; more {
		t.Error("outcome channel emitted a second value; want exactly one")
	}
	return oc, nil
}

// TestConformanceAgentDroidBYOKLive is the release gate for the as-integrated
// BYOK path: the real `droid` binary, launched through the native backend via
// the prelude + backend.Exec chain, must dial the BYOK base_url with the
// process-env-expanded API key in the Authorization header. The mock gateway is
// an in-process httptest.Server; no Factory account / FACTORY_API_KEY is needed.
//
// Gate: droid on PATH AND AWF_DROID_BYOK_LIVE set. (No FACTORY_API_KEY gate —
// BYOK is keyless w.r.t. Factory.)
func TestConformanceAgentDroidBYOKLive(t *testing.T) {
	skipIfNoDroid(t)
	if os.Getenv("AWF_DROID_BYOK_LIVE") == "" {
		t.Skip("AWF_DROID_BYOK_LIVE not set; skipping real-binary droid BYOK smoke")
	}
	t.Setenv(byokTestKeyEnv, byokTestKeyValue)

	// model strings cover the real client shapes: plain, slash-namespaced, and
	// slash+colon (provider/family:tag). Each must arrive at the gateway verbatim.
	models := []string{
		"claude-sonnet-4-6",
		"bedrock/anthropic.claude-3",
		"ollama/llama3:8b",
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			var mu sync.Mutex
			var recorded []recordedRequest

			srv := httptest.NewServer(byokMockHandler(t, &mu, &recorded))
			defer srv.Close()

			oc, err := runDroidBYOK(t, srv.URL+"/v1", model, false)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			assertGatewayDialed(t, recorded, model)

			// The mock returns a valid completion, so droid should finish OK. If
			// droid surfaces a non-nil err here despite the gateway having been
			// dialed correctly, that is a droid-behavior mismatch worth surfacing —
			// but the load-bearing proof (the recorded Bearer header) is above.
			if oc.Err != nil {
				t.Errorf("Launch outcome err (gateway WAS dialed correctly; droid still failed): %v", oc.Err)
			}
		})
	}
}

// TestConformanceAgentDroidBYOKLiveTLS is the optional TLS variant: an
// httptest.NewTLSServer (self-signed cert) + tls_insecure:true, proving droid
// still connects via the NODE_TLS_REJECT_UNAUTHORIZED=0 path. Same gate.
func TestConformanceAgentDroidBYOKLiveTLS(t *testing.T) {
	skipIfNoDroid(t)
	if os.Getenv("AWF_DROID_BYOK_LIVE") == "" {
		t.Skip("AWF_DROID_BYOK_LIVE not set; skipping real-binary droid BYOK TLS smoke")
	}
	t.Setenv(byokTestKeyEnv, byokTestKeyValue)

	const model = "claude-sonnet-4-6"

	var mu sync.Mutex
	var recorded []recordedRequest

	srv := httptest.NewTLSServer(byokMockHandler(t, &mu, &recorded))
	defer srv.Close()

	oc, err := runDroidBYOK(t, srv.URL+"/v1", model, true)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	assertGatewayDialed(t, recorded, model)
	if oc.Err != nil {
		t.Errorf("Launch outcome err over TLS (gateway WAS dialed; droid still failed): %v", oc.Err)
	}
}

// assertGatewayDialed verifies the recorded requests prove the end-to-end BYOK
// flow: a POST to …/v1/chat/completions carrying the expanded Bearer key and
// the exact model string. Caller holds mu.
func assertGatewayDialed(t *testing.T, recorded []recordedRequest, wantModel string) {
	t.Helper()
	if len(recorded) == 0 {
		t.Fatal("mock gateway recorded no requests; droid never dialed base_url")
	}

	var completion *recordedRequest
	for i := range recorded {
		r := recorded[i]
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/chat/completions") {
			completion = &recorded[i]
			break
		}
	}
	if completion == nil {
		t.Fatalf("no POST .../chat/completions among recorded requests: %+v", recorded)
	}

	if !strings.HasSuffix(completion.Path, "/v1/chat/completions") {
		t.Errorf("completion path = %q; want suffix /v1/chat/completions", completion.Path)
	}
	wantAuth := "Bearer " + byokTestKeyValue
	if completion.Auth != wantAuth {
		t.Errorf("Authorization = %q; want %q (the ${%s} placeholder must have been expanded from droid's process env)", completion.Auth, wantAuth, byokTestKeyEnv)
	}
	if completion.Model != wantModel {
		t.Errorf("request body model = %q; want %q (the exact model string, slash/colon preserved)", completion.Model, wantModel)
	}
	t.Logf("gateway dialed: %s %s | Authorization=%q | model=%q", completion.Method, completion.Path, completion.Auth, completion.Model)
}
