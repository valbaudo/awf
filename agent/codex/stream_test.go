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

// agentMsgStarted is an item.started carrying an agent_message item — the
// discriminator must return false because only item.completed items carry
// the final text.
func agentMsgStarted(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "item.started",
		"item": map[string]any{"id": "item_0", "type": "agent_message", "text": text},
	})
	return b
}

func TestAgentMessageText_Discriminates(t *testing.T) {
	// item.completed / agent_message → (text, true)
	ev, err := codex.ParseStreamEventForTest(itemMsg("hello"))
	if err != nil {
		t.Fatalf("parse itemMsg: %v", err)
	}
	if txt, ok := codex.AgentMessageTextForTest(ev); !ok || txt != "hello" {
		t.Errorf("itemMsg: got (%q, %v), want (\"hello\", true)", txt, ok)
	}

	// item.completed / command_execution → ("", false)
	ev, err = codex.ParseStreamEventForTest(cmdCompleted("ls", "out\n", 0))
	if err != nil {
		t.Fatalf("parse cmdCompleted: %v", err)
	}
	if txt, ok := codex.AgentMessageTextForTest(ev); ok {
		t.Errorf("cmdCompleted: got (%q, true), want (\"\", false)", txt)
	}

	// item.started / command_execution → ("", false)
	ev, err = codex.ParseStreamEventForTest(cmdStarted("ls"))
	if err != nil {
		t.Fatalf("parse cmdStarted: %v", err)
	}
	if txt, ok := codex.AgentMessageTextForTest(ev); ok {
		t.Errorf("cmdStarted: got (%q, true), want (\"\", false)", txt)
	}

	// item.started / agent_message → ("", false): only item.completed carries final text
	ev, err = codex.ParseStreamEventForTest(agentMsgStarted("partial"))
	if err != nil {
		t.Fatalf("parse agentMsgStarted: %v", err)
	}
	if txt, ok := codex.AgentMessageTextForTest(ev); ok {
		t.Errorf("agentMsgStarted: got (%q, true), want (\"\", false)", txt)
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

func TestBuildResult_StrictSchema_AllowsOnlyWholeJSONDocument(t *testing.T) {
	schema := &ir.JSONSchema{"type": "object"}
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: schema}

	res, err := codex.BuildResultForTest("\n\t {\"answer\":4} \n", nil, inv)
	if err != nil {
		t.Fatalf("buildResult(whitespace-wrapped JSON): %v", err)
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4", res.Output["answer"])
	}

	cases := []struct {
		name string
		text string
	}{
		{name: "prose-prefix", text: `answer: {"answer":4}`},
		{name: "markdown-fence", text: "```json\n{\"answer\":4}\n```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codex.BuildResultForTest(tc.text, nil, inv)
			var unp *agent.ErrUnparseableOutput
			if err == nil || !errors.As(err, &unp) {
				t.Fatalf("buildResult(%q) = %v, want *agent.ErrUnparseableOutput", tc.text, err)
			}
		})
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
	if res.Metrics.Cost.Total != 0 {
		t.Errorf("cost = %v, want 0", res.Metrics.Cost.Total)
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

func TestEventKind(t *testing.T) {
	cases := []struct {
		raw  []byte
		want string
	}{
		{itemMsg("hi"), "agent_message"},
		{cmdStarted("ls"), "command_execution"},
		{turnCompleted(1, 0, 1), "turn.completed"},
		{errorEvent(apiErr400), "error"},
	}
	for _, c := range cases {
		ev, err := codex.ParseStreamEventForTest(c.raw)
		if err != nil {
			t.Fatalf("parse %s: %v", c.raw, err)
		}
		if got := codex.EventKindForTest(ev); got != c.want {
			t.Errorf("eventKind = %q, want %q (raw: %s)", got, c.want, c.raw)
		}
	}
}
