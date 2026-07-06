package awfllm_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
)

func TestRefAndCapabilities(t *testing.T) {
	a, err := awfllm.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Ref() != "awf/llm" {
		t.Errorf("Ref() = %q, want %q", a.Ref(), "awf/llm")
	}
	caps := a.Capabilities()
	if caps.NativeSchema {
		t.Error("NativeSchema = true, want false (layer-2: adapter parses message content)")
	}
	if !caps.Containerless {
		t.Error("Containerless = false, want true (direct HTTP, no container)")
	}
	if !caps.Threaded {
		t.Error("Threaded = false, want true (adapter supports engine-supplied continues: threads)")
	}
}

func TestWithEnv_EmptyOK(t *testing.T) {
	if _, err := awfllm.New(awfllm.WithEnv(nil)); err != nil {
		t.Fatalf("New(WithEnv(nil)): %v", err)
	}
}

func TestAdapterSatisfiesToolLoopRunner(t *testing.T) {
	var _ agent.ToolLoopRunner = (*awfllm.Adapter)(nil) // compile-time
}

// TestAdapterRunToolLoopOneRound drives RunToolLoop through the injected fake
// client and asserts one round returns the parsed tool_call result (Task 3.5).
func TestAdapterRunToolLoopOneRound(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return sseResponse(toolCallSSE), nil
	})
	a, _ := awfllm.New(
		awfllm.WithHTTPClient(&http.Client{Transport: rt}),
		awfllm.WithEnv(map[string]string{"OPENAI_API_KEY": "sk-test"}),
	)
	res, err := a.RunToolLoop(context.Background(), agent.ToolLoopInvocation{
		NodePath: "react[0].round-1.model",
		Uses:     "awf/llm",
		With:     ir.RawConfig{"model": "m", "base_url": "https://x/v1"},
		Messages: []agent.ReactTurn{{Role: "user", Content: "q"}},
		Tools:    []agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "check" {
		t.Fatalf("RunToolLoop res = %+v", res)
	}
}

// TestAdapterRunToolLoopRejectsOllama: the Ollama-native path cannot drive a tool
// loop (no /api/chat tool support in v1) — RunToolLoop must reject it (Task 3.5).
func TestAdapterRunToolLoopRejectsOllama(t *testing.T) {
	a, _ := awfllm.New(awfllm.WithEnv(map[string]string{"OPENAI_API_KEY": "sk-test"}))
	_, err := a.RunToolLoop(context.Background(), agent.ToolLoopInvocation{
		Uses:     "awf/llm",
		With:     ir.RawConfig{"model": "m", "structured_output": "ollama_format"},
		Messages: []agent.ReactTurn{{Role: "user", Content: "q"}},
		Tools:    []agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err == nil {
		t.Fatal("RunToolLoop must reject structured_output:ollama_format")
	}
}

// TestAdapterRunToolLoopValidates: validateConfigForToolLoop runs first — a
// rejected key (tools) must surface before any HTTP call (Task 3.5).
func TestAdapterRunToolLoopValidates(t *testing.T) {
	a, _ := awfllm.New(awfllm.WithEnv(map[string]string{"OPENAI_API_KEY": "sk-test"}))
	_, err := a.RunToolLoop(context.Background(), agent.ToolLoopInvocation{
		Uses:     "awf/llm",
		With:     ir.RawConfig{"model": "m", "tools": []any{}},
		Messages: []agent.ReactTurn{{Role: "user", Content: "q"}},
	})
	if err == nil {
		t.Fatal("RunToolLoop must reject a tools with-key via validateConfigForToolLoop")
	}
}

func TestRunToolLoop_RejectsAnthropic(t *testing.T) {
	a, _ := awfllm.New(awfllm.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "k"}))
	_, err := a.RunToolLoop(context.Background(), agent.ToolLoopInvocation{
		With: ir.RawConfig{"provider": "anthropic", "model": "claude-sonnet-4-6"},
	})
	if err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("react: on provider: anthropic must be rejected in v1, got %v", err)
	}
}

// TestDefaultEnvAllowlist_IncludesGemini (F36): GEMINI_API_KEY is the documented
// default api_key_env for provider: gemini — it must be forwarded to the
// containerless HTTP call like OPENAI_API_KEY/ANTHROPIC_API_KEY are.
func TestDefaultEnvAllowlist_IncludesGemini(t *testing.T) {
	found := false
	for _, k := range awfllm.DefaultEnvAllowlist {
		if k == "GEMINI_API_KEY" {
			found = true
		}
	}
	if !found {
		t.Errorf("DefaultEnvAllowlist must include GEMINI_API_KEY, got %v", awfllm.DefaultEnvAllowlist)
	}
}

// TestRequiredEnv_ReturnsFullAllowlist (F36): RequiredEnv must return the same
// full forward-allowlist as DefaultEnvAllowlist, so the credential-presence
// preflight does not false-warn for a Gemini-only (or Anthropic-only) user.
func TestRequiredEnv_ReturnsFullAllowlist(t *testing.T) {
	got := (&awfllm.Adapter{}).RequiredEnv()
	if len(got) != len(awfllm.DefaultEnvAllowlist) {
		t.Fatalf("RequiredEnv() = %v, want len == len(DefaultEnvAllowlist) = %v", got, awfllm.DefaultEnvAllowlist)
	}
}

// TestAwfllm_RequiredEnvReturnsCopy (F13): RequiredEnv must return a COPY, not
// the shared DefaultEnvAllowlist package var — a caller must not be able to
// corrupt it. Checks backing-array identity directly (deterministic; no
// reliance on append's capacity-dependent realloc behavior).
func TestAwfllm_RequiredEnvReturnsCopy(t *testing.T) {
	got := (&awfllm.Adapter{}).RequiredEnv()
	if len(got) == 0 {
		t.Fatal("RequiredEnv returned empty")
	}
	// A copy must NOT share DefaultEnvAllowlist's backing array.
	if &got[0] == &awfllm.DefaultEnvAllowlist[0] {
		t.Fatal("RequiredEnv aliases DefaultEnvAllowlist's backing array; must return a copy")
	}
}
