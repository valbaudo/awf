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
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, emit)
}

// ReqConfigForTest mirrors reqConfig (unexported) for test construction.
type ReqConfigForTest = reqConfig

var AssemblePromptForTest = assemblePrompt

// ClientForTest exposes clientFor for white-box tests (tls_insecure behavior).
func (a *Adapter) ClientForTest(insecure bool) *http.Client { return a.clientFor(insecure) }
