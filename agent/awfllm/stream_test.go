package awfllm_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
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
	res, err := awfllm.BuildResultForTest(`{"answer":4}`, awfllm.NewUsageForTest(20, 5, 2), "", pricing.Default(), inv)
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
	res, err := awfllm.BuildResultForTest("free text", awfllm.NewUsageForTest(1, 1, 0), "", pricing.Default(), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil (no schema)", res.Output)
	}
}

func TestBuildResult_Unparseable(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: &ir.JSONSchema{"type": "object"}}
	_, err := awfllm.BuildResultForTest("not json", awfllm.NewUsageForTest(0, 0, 0), "", pricing.Default(), inv)
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
	res, err := awfllm.BuildResultForTest("the draft text", awfllm.NewUsageForTest(10, 5, 0), "", pricing.Default(), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	want := agent.ThreadTurn{User: "write a draft", Assistant: "the draft text"}
	if res.Transcript != want {
		t.Errorf("Transcript = %+v, want %+v", res.Transcript, want)
	}
}

func TestBuildResult_TranscriptExcludesContextEvidence(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath: "gate[0].attempt-1.evaluate.judge",
		With:     ir.RawConfig{"prompt": "judge the candidate"},
		ContextEvidence: []agent.ThreadTurn{
			{User: "source user", Assistant: "source answer"},
		},
	}
	res, err := awfllm.BuildResultForTest("approved", awfllm.NewUsageForTest(10, 5, 0), "", pricing.Default(), inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Transcript.User != "judge the candidate" {
		t.Fatalf("Transcript.User = %q, want clean prompt", res.Transcript.User)
	}
	if strings.Contains(res.Transcript.User, "awf_source_context") || strings.Contains(res.Transcript.User, "source user") {
		t.Fatalf("Transcript.User contains context evidence: %q", res.Transcript.User)
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
	res, err := awfllm.BuildResultForTest(full, awfllm.NewUsageForTest(10, 5, 0), "", pricing.Default(), inv)
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

func TestMetricsFrom_PerProviderCacheNormalization(t *testing.T) {
	tbl := pricing.Table{"claude-sonnet-4-6": {Currency: "USD", InputPerM: 3, OutputPerM: 15, CacheReadPerM: 0.3, CacheWritePerM: 3.75}}
	const eps = 1e-9

	// OpenAI/Gemini semantics (anthropicNorm=false): prompt count INCLUDES cached → subtract.
	openai := awfllm.MetricsFromForTest(1_000_000, 0, 200_000, 0, "claude-sonnet-4-6", tbl, false)
	// cost.Input = (1_000_000-200_000)/1e6*3 + 200_000/1e6*0.3 = 2.4 + 0.06 = 2.46
	if math.Abs(openai.Cost.Input-2.46) > eps {
		t.Errorf("non-anthropic Input cost = %v, want 2.46 (subtract cached)", openai.Cost.Input)
	}

	// Anthropic semantics (anthropicNorm=true): input_tokens EXCLUDES cache → no subtract.
	anth := awfllm.MetricsFromForTest(1_000_000, 0, 200_000, 100_000, "claude-sonnet-4-6", tbl, true)
	// cost.Input = 1_000_000/1e6*3 + 200_000/1e6*0.3 + 100_000/1e6*3.75 = 3 + 0.06 + 0.375 = 3.435
	if math.Abs(anth.Cost.Input-3.435) > eps {
		t.Errorf("anthropic Input cost = %v, want 3.435 (no subtract, +cache write)", anth.Cost.Input)
	}
	if anth.Tokens.CacheCreationInput != 100_000 {
		t.Errorf("CacheCreationInput = %d, want 100000", anth.Tokens.CacheCreationInput)
	}
	if anth.Tokens.CacheReadInput != 200_000 {
		t.Errorf("CacheReadInput = %d, want 200000", anth.Tokens.CacheReadInput)
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

// TestBuildResult_DerivedCost_Hit verifies that when the wire model is known in the
// injected pricing table, buildResult derives a USD cost with:
//   - Metrics.Model == wire model
//   - Cost.Source == agent.CostSourceDerived
//   - Cost.Input normalized: (Input - CacheRead) * InputPerM/1e6 + CacheRead * CacheReadPerM/1e6
//   - Cost.Output == Output * OutputPerM/1e6
//   - Cost.Total == Cost.Input + Cost.Output (exact)
func TestBuildResult_DerivedCost_Hit(t *testing.T) {
	fixtureTable := pricing.Table{
		"gpt-5.3-codex": {Currency: "USD", InputPerM: 2, OutputPerM: 10, CacheReadPerM: 0.5},
	}
	// Input 1_000_000 total, CacheRead 200_000 → normalized input = 800_000, cache = 200_000
	// Cost.Input = 800_000*2/1e6 + 200_000*0.5/1e6 = 1.6 + 0.1 = 1.7
	// Cost.Output = 1_000_000*10/1e6 = 10.0
	usage := awfllm.NewUsageForTest(1_000_000, 1_000_000, 200_000)
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"prompt": "q"}}
	res, err := awfllm.BuildResultForTest(`{"x":1}`, usage, "gpt-5.3-codex", fixtureTable, inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Model != "gpt-5.3-codex" {
		t.Errorf("Metrics.Model = %q, want %q", res.Metrics.Model, "gpt-5.3-codex")
	}
	if res.Metrics.Cost.Source != agent.CostSourceDerived {
		t.Errorf("Cost.Source = %q, want %q", res.Metrics.Cost.Source, agent.CostSourceDerived)
	}
	const eps = 1e-9
	wantInput := 1.7
	wantOutput := 10.0
	if math.Abs(res.Metrics.Cost.Input-wantInput) > eps {
		t.Errorf("Cost.Input = %v, want %v (eps %v)", res.Metrics.Cost.Input, wantInput, eps)
	}
	if math.Abs(res.Metrics.Cost.Output-wantOutput) > eps {
		t.Errorf("Cost.Output = %v, want %v (eps %v)", res.Metrics.Cost.Output, wantOutput, eps)
	}
	wantTotal := res.Metrics.Cost.Input + res.Metrics.Cost.Output
	if res.Metrics.Cost.Total != wantTotal {
		t.Errorf("Cost.Total = %v, want %v (must equal Input+Output exactly)", res.Metrics.Cost.Total, wantTotal)
	}
}

// TestBuildResult_DerivedCost_Miss verifies that an unknown model results in an
// absent cost (Source == "") — never $0 with Source set.
func TestBuildResult_DerivedCost_Miss(t *testing.T) {
	fixtureTable := pricing.Table{
		"gpt-5.3-codex": {Currency: "USD", InputPerM: 2, OutputPerM: 10, CacheReadPerM: 0.5},
	}
	usage := awfllm.NewUsageForTest(100, 50, 0)
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"prompt": "q"}}
	res, err := awfllm.BuildResultForTest("hello", usage, "unknown-model-xyz", fixtureTable, inv)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Cost.Source != "" {
		t.Errorf("Cost.Source = %q, want empty (absent) for unknown model", res.Metrics.Cost.Source)
	}
	if res.Metrics.Model != "unknown-model-xyz" {
		t.Errorf("Metrics.Model = %q, want %q (model set even on miss)", res.Metrics.Model, "unknown-model-xyz")
	}
}
