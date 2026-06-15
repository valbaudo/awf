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
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, nil, nil, emit)
}

func (a *Adapter) StreamWithContextForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, contextEvidence []agent.ThreadTurn, emit func(string, []byte)) (string, usageRec, string, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, contextEvidence, nil, emit)
}

// StreamWithFilesForTest threads input files through stream for the OpenAI
// content-part wire test (Task 7).
func (a *Adapter) StreamWithFilesForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(string, []byte)) (string, usageRec, string, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, nil, files, emit)
}

func (a *Adapter) StreamWithFilesAndContextForTest(ctx context.Context, cfg ReqConfigForTest, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, contextEvidence []agent.ThreadTurn, emit func(string, []byte)) (string, usageRec, string, string, error) {
	return a.stream(ctx, reqConfig(cfg), prompt, schema, thread, contextEvidence, files, emit)
}

// ReqConfigForTest mirrors reqConfig (unexported) for test construction.
type ReqConfigForTest = reqConfig

// BuildReqConfigForTest exposes buildReqConfig for white-box config tests.
func (a *Adapter) BuildReqConfigForTest(inv agent.AgentInvocation) (ReqConfigForTest, error) {
	return a.buildReqConfig(inv)
}

var AssemblePromptForTest = assemblePrompt

var RenderContextEvidenceForTest = renderContextEvidence

// EffectiveProviderForTest + ProviderDefaultsForTest expose the single-source
// transport-selection helpers for the drift/consistency white-box test.
var (
	EffectiveProviderForTest = effectiveProvider
	ProviderDefaultsForTest  = providerDefaults
)

// ClientForTest exposes clientFor for white-box tests (tls_insecure behavior).
func (a *Adapter) ClientForTest(insecure bool) *http.Client { return a.clientFor(insecure) }

// ValidateConfigForToolLoopForTest exposes the prompt-exempt validate variant.
func (a *Adapter) ValidateConfigForToolLoopForTest(with ir.RawConfig) error {
	return a.validateConfigForToolLoop(with)
}

// MetricsFromForTest exercises the per-provider cache normalization. usageRec is
// unexported, so the helper builds it from explicit counts.
func MetricsFromForTest(in, out, cacheRead, cacheWrite int, model string, pricer pricing.Table, anthropicNorm bool) agent.MetricSet {
	return metricsFrom(usageRec{Input: in, Output: out, CacheRead: cacheRead, CacheWrite: cacheWrite}, model, pricer, anthropicNorm)
}

// RunOneToolCallForTest exposes the unexported runOneToolCall for white-box wire
// tests (the canned-stream harness).
func (a *Adapter) RunOneToolCallForTest(ctx context.Context, cfg ReqConfigForTest, nodePath string, msgs []agent.ReactTurn, tools []agent.ToolDef, schema *ir.JSONSchema) (agent.ToolLoopResult, error) {
	return a.runOneToolCall(ctx, reqConfig(cfg), nodePath, msgs, tools, schema)
}

// BuildAnthropicBodyForTest exposes the Anthropic request-body builder.
func BuildAnthropicBodyForTest(cfg ReqConfigForTest, prompt string, thread []agent.ThreadTurn, contextEvidence []agent.ThreadTurn, files []agent.InputFile) (map[string]any, error) {
	return buildAnthropicBody(reqConfig(cfg), prompt, thread, contextEvidence, files)
}

// GeminiCacheKeyForTest exposes the content-address key function.
func GeminiCacheKeyForTest(model, systemPrompt string, files []agent.InputFile) string {
	return geminiCacheKey(model, systemPrompt, files)
}

// EnsureGeminiCacheForTest exposes the cache-lifecycle helper.
func EnsureGeminiCacheForTest(a *Adapter, ctx context.Context, cfg ReqConfigForTest, files []agent.InputFile) (string, error) {
	return a.ensureGeminiCache(ctx, reqConfig(cfg), files)
}

// GeminiCacheConfigForTest builds an explicit-mode gemini cache config for tests.
func GeminiCacheConfigForTest(mode, ttl string) *geminiCacheConfig {
	return &geminiCacheConfig{Mode: mode, TTL: ttl}
}
