package agent

import "testing"

func TestToolLoopTypesExist(t *testing.T) {
	inv := ToolLoopInvocation{
		NodePath: "react[0].round-1.model",
		Uses:     "awf/llm",
		Messages: []ReactTurn{{Role: "user", Content: "hi"}},
		Tools:    []ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}
	res := ToolLoopResult{
		Text:         "",
		ToolCalls:    []ToolCall{{Index: 0, ID: "call_1", Name: "check", Arguments: `{"x":1}`}},
		FinishReason: "tool_calls",
	}
	if inv.Messages[0].Role != "user" || res.ToolCalls[0].ID != "call_1" {
		t.Fatal("type wiring wrong")
	}
}
