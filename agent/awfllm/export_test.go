package awfllm

import (
	"context"
	"net/http"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

// Exports for white-box tests (test files are package awfllm_test).
var (
	ExtractJSONObjectForTest   = extractJSONObject
	IsPermanentLLMErrorForTest = isPermanentLLMError
)

// BuildResultForTest exposes buildResult with explicit model and pricer for tests.
func BuildResultForTest(full string, usage usageRec, model string, pricer pricing.Table, inv agent.AgentInvocation) (agent.AgentResult, error) {
	return buildResult(full, usage, model, pricer, inv)
}

func NewUsageForTest(in, out, cached int) usageRec {
	return usageRec{Input: in, Output: out, CacheRead: cached}
}

func NewAPIErrorForTest(status int, typ, body string) *apiError {
	return &apiError{Status: status, Type: typ, Body: body}
}

func (a *Adapter) StreamForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, emit func(string, []byte)) (string, usageRec, string, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, nil, emit)
}

// StreamWithFilesForTest threads input files through stream for the OpenAI
// content-part wire test (Task 7).
func (a *Adapter) StreamWithFilesForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(string, []byte)) (string, usageRec, string, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, files, emit)
}

// ReqConfigForTest mirrors reqConfig (unexported) for test construction.
type ReqConfigForTest = reqConfig

// BuildReqConfigForTest exposes buildReqConfig for white-box config tests.
func (a *Adapter) BuildReqConfigForTest(inv agent.AgentInvocation) (ReqConfigForTest, error) {
	return a.buildReqConfig(inv)
}

var AssemblePromptForTest = assemblePrompt

// ClientForTest exposes clientFor for white-box tests (tls_insecure behavior).
func (a *Adapter) ClientForTest(insecure bool) *http.Client { return a.clientFor(insecure) }

// ValidateConfigForToolLoopForTest exposes the prompt-exempt validate variant.
func (a *Adapter) ValidateConfigForToolLoopForTest(with ir.RawConfig) error {
	return a.validateConfigForToolLoop(with)
}

// RunOneToolCallForTest exposes the unexported runOneToolCall for white-box wire
// tests (the canned-stream harness).
func (a *Adapter) RunOneToolCallForTest(ctx context.Context, cfg ReqConfigForTest, nodePath string, msgs []agent.ReactTurn, tools []agent.ToolDef, schema *ir.JSONSchema) (agent.ToolLoopResult, error) {
	return a.runOneToolCall(ctx, reqConfig(cfg), nodePath, msgs, tools, schema)
}
