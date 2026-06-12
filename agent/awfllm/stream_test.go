package awfllm_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
)

func TestExtractJSONObject_FenceAndProse(t *testing.T) {
	got, err := awfllm.ExtractJSONObjectForTest("here you go:\n```json\n{\"answer\":4}\n```\nthanks")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got["answer"].(float64) != 4 {
		t.Errorf("answer = %v, want 4", got["answer"])
	}
}

func TestExtractJSONObject_LastObjectWins(t *testing.T) {
	got, err := awfllm.ExtractJSONObjectForTest(`{"answer":1} then reconsidering {"answer":4}`)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got["answer"].(float64) != 4 {
		t.Errorf("answer = %v, want 4 (last wins)", got["answer"])
	}
}

func TestBuildResult_SchemaParsesReassembledText(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: &ir.JSONSchema{"type": "object"}}
	// full text reassembled from deltas: `{"` + `answer":4}`
	res, err := awfllm.BuildResultForTest(`{"answer":4}`, awfllm.NewUsageForTest(20, 5, 2), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Output["answer"].(float64) != 4 {
		t.Errorf("Output[answer] = %v, want 4", res.Output["answer"])
	}
	if res.Metrics.Tokens.Input != 20 || res.Metrics.Tokens.Output != 5 || res.Metrics.Tokens.CacheReadInput != 2 {
		t.Errorf("tokens = %+v", res.Metrics.Tokens)
	}
	if res.Metrics.Cost.Total != 0 || res.Metrics.Turns != 1 {
		t.Errorf("cost=%v turns=%d, want 0 and 1", res.Metrics.Cost.Total, res.Metrics.Turns)
	}
}

func TestBuildResult_NoSchema_NilOutput(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]"}
	res, err := awfllm.BuildResultForTest("free text", awfllm.NewUsageForTest(1, 1, 0), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil (no schema)", res.Output)
	}
}

func TestBuildResult_Unparseable(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: &ir.JSONSchema{"type": "object"}}
	_, err := awfllm.BuildResultForTest("not json", awfllm.NewUsageForTest(0, 0, 0), inv)
	var unp *agent.ErrUnparseableOutput
	if !errors.As(err, &unp) {
		t.Fatalf("buildResult(non-JSON) = %v, want *agent.ErrUnparseableOutput", err)
	}
}

// TestBuildResult_Transcript_D13_FreeText verifies that buildResult stamps the
// Transcript pair: User = clean with["prompt"], Assistant = verbatim full (prose).
func TestBuildResult_Transcript_D13_FreeText(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		With:     ir.RawConfig{"prompt": "write a draft"},
	}
	res, err := awfllm.BuildResultForTest("the draft text", awfllm.NewUsageForTest(10, 5, 0), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	want := agent.ThreadTurn{User: "write a draft", Assistant: "the draft text"}
	if res.Transcript != want {
		t.Errorf("Transcript = %+v, want %+v", res.Transcript, want)
	}
}

// TestBuildResult_Transcript_D13_Typed verifies that for a typed turn (output_schema
// set) Transcript.Assistant is the verbatim full JSON string — NOT extracted/parsed —
// and Transcript.User is the clean prompt.
func TestBuildResult_Transcript_D13_Typed(t *testing.T) {
	schema := &ir.JSONSchema{"type": "object"}
	inv := agent.AgentInvocation{
		NodePath:     "graph[0]",
		OutputSchema: schema,
		With:         ir.RawConfig{"prompt": "generate data"},
	}
	full := `{"draft":"x"}`
	res, err := awfllm.BuildResultForTest(full, awfllm.NewUsageForTest(10, 5, 0), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Transcript.User != "generate data" {
		t.Errorf("Transcript.User = %q, want %q", res.Transcript.User, "generate data")
	}
	if res.Transcript.Assistant != full {
		t.Errorf("Transcript.Assistant = %q, want verbatim %q", res.Transcript.Assistant, full)
	}
}

func TestIsPermanentLLMError(t *testing.T) {
	if !awfllm.IsPermanentLLMErrorForTest(awfllm.NewAPIErrorForTest(400, "invalid_request_error", "bad model")) {
		t.Error("400 + invalid_request_error should be permanent")
	}
	if awfllm.IsPermanentLLMErrorForTest(awfllm.NewAPIErrorForTest(429, "rate_limit_error", "slow down")) {
		t.Error("429 must be retryable")
	}
	if awfllm.IsPermanentLLMErrorForTest(errors.New("transport reset")) {
		t.Error("plain transport error must be retryable")
	}
}
