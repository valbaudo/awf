package codex_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/ir"
)

// ---- codex --json line builders (shared by stream_test + launch_test) --------

func itemMsg(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"id": "item_0", "type": "agent_message", "text": text},
	})
	return b
}

func cmdStarted(command string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "item.started",
		"item": map[string]any{"id": "item_1", "type": "command_execution", "command": command, "status": "in_progress"},
	})
	return b
}

func cmdCompleted(command, out string, exit int) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"id": "item_1", "type": "command_execution", "command": command, "aggregated_output": out, "exit_code": exit, "status": "completed"},
	})
	return b
}

func turnCompleted(in, cached, out int) []byte {
	return []byte(fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0}}`, in, cached, out))
}

// turnFailed/errorEvent embed apiErr (itself a JSON-encoded API error string) as
// the error.message / message value — exactly codex's wire shape.
func turnFailed(apiErr string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]any{"message": apiErr}})
	return b
}

func errorEvent(apiErr string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "message": apiErr})
	return b
}

const apiErr400 = `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'x' model is not supported"}}`
const apiErr429 = `{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"slow down — invalid_request_error appears here as a decoy"}}`

// ---- tests -------------------------------------------------------------------

func TestParse_AgentMessage_LastWins(t *testing.T) {
	// A premature agent_message, a command, then the final agent_message: last-wins.
	for _, raw := range [][]byte{itemMsg(`{"answer":1}`), cmdCompleted("ls", "a\nb\n", 0), itemMsg(`{"answer":4}`)} {
		ev, err := codex.ParseStreamEventForTest(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if txt, ok := codex.AgentMessageTextForTest(ev); ok {
			_ = txt // exercised; last-wins selection is asserted end-to-end in launch_test
		}
	}
}

func TestBuildResult_StrictSchema(t *testing.T) {
	schema := &ir.JSONSchema{"type": "object"}
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: schema}
	res, err := codex.BuildResultForTest(`{"answer":4}`, nil, inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4", res.Output["answer"])
	}
}

func TestBuildResult_NonJSON_Unparseable(t *testing.T) {
	schema := &ir.JSONSchema{"type": "object"}
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: schema}
	_, err := codex.BuildResultForTest("not json at all", nil, inv)
	var unp *agent.ErrUnparseableOutput
	if err == nil || !errors.As(err, &unp) {
		t.Fatalf("buildResult(non-JSON) = %v, want *agent.ErrUnparseableOutput", err)
	}
}

func TestBuildResult_NoSchema_NilOutput_TokensCostZero(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]"} // no schema
	res, err := codex.BuildResultForTest("free text answer", codex.NewUsageForTest(10, 3, 5), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil (no schema)", res.Output)
	}
	if res.Metrics.Tokens.Input != 10 || res.Metrics.Tokens.Output != 5 || res.Metrics.Tokens.CacheReadInput != 3 {
		t.Errorf("tokens = %+v, want Input:10 Output:5 CacheReadInput:3", res.Metrics.Tokens)
	}
	if res.Metrics.Cost.USD != 0 {
		t.Errorf("cost = %v, want 0", res.Metrics.Cost.USD)
	}
}

func TestIsPermanentCodexError(t *testing.T) {
	if !codex.IsPermanentCodexErrorForTest(apiErr400) {
		t.Error("status 400 + invalid_request_error should be permanent")
	}
	if codex.IsPermanentCodexErrorForTest(apiErr429) {
		t.Error("status 429 must be retryable even though body embeds 'invalid_request_error'")
	}
	if codex.IsPermanentCodexErrorForTest("not json") {
		t.Error("unparseable message must be retryable (false)")
	}
}
