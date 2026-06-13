package awfllm_test

import (
	"context"
	"net/http"
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
