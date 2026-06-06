package awfllm

import (
	"context"
	"net/http"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// Exports for white-box tests (test files are package awfllm_test).
var (
	ExtractJSONObjectForTest   = extractJSONObject
	BuildResultForTest         = buildResult
	IsPermanentLLMErrorForTest = isPermanentLLMError
)

func NewUsageForTest(in, out, cached int) usageRec {
	return usageRec{Input: in, Output: out, CacheRead: cached}
}

func NewAPIErrorForTest(status int, typ, body string) *apiError {
	return &apiError{Status: status, Type: typ, Body: body}
}

func (a *Adapter) StreamForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, emit func(string, []byte)) (string, usageRec, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, emit)
}

// ReqConfigForTest mirrors reqConfig (unexported) for test construction.
type ReqConfigForTest = reqConfig

var AssemblePromptForTest = assemblePrompt

// ClientForTest exposes clientFor for white-box tests (tls_insecure behavior).
func (a *Adapter) ClientForTest(insecure bool) *http.Client { return a.clientFor(insecure) }
