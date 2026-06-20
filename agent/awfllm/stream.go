package awfllm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/pricing"
)

// usageRec is the normalized token usage every transport fills. CacheRead maps to
// MetricTokens.CacheReadInput; CacheWrite (Anthropic cache_creation_input_tokens)
// maps to MetricTokens.CacheCreationInput. Zero when a backend omits usage.
type usageRec struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int // Anthropic cache_creation_input_tokens (0 elsewhere)
}

// apiError is the uniform HTTP error both transports produce, so classification
// is wire-shaped (status + type), not SDK-symbol-shaped. The OpenAI path maps
// *openai.Error → apiError; the Ollama path maps a non-2xx response → apiError.
type apiError struct {
	Status int
	Type   string
	Code   string // provider error.code (OpenAI); "" for synthesized non-OpenAI types
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("agent/awfllm: HTTP %d (%s): %s", e.Status, e.Type, e.Body)
}

// errTypeInvalidRequest is the OpenAI/Ollama error type string for a permanent
// client-side request fault (bad model, unsupported parameter, schema rejection).
const errTypeInvalidRequest = "invalid_request_error"

// isPermanentLLMError reports a permanent client-side fault: 400 +
// invalid_request_error, OR a quota/budget-exhausted response (which retry can
// never clear). Quota is matched by structured type/code where available (raw
// OpenAI: insufficient_quota) and by a message-substring fallback that is
// REQUIRED for the LiteLLM-wrapped case (LiteLLM re-wraps OpenAI's
// insufficient_quota into a RateLimitError whose body buries the token). NOT
// gated on status (OpenAI quota is 429; LiteLLM budget is 400-or-429). Plain
// rate-limit (rate_limit_exceeded), 5xx, and transport faults stay retryable.
func isPermanentLLMError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == 400 && ae.Type == errTypeInvalidRequest {
		return true
	}
	return isQuotaOrBudget(ae)
}

func isQuotaOrBudget(ae *apiError) bool {
	if ae.Type == "insufficient_quota" || ae.Code == "insufficient_quota" {
		return true
	}
	for _, s := range []string{
		"insufficient_quota",
		"exceeded your current quota",
		"Budget has been exceeded",
		"ExceededBudget",
		"BudgetExceededError",
	} {
		if strings.Contains(ae.Body, s) {
			return true
		}
	}
	return false
}

// buildResult parses the reassembled full text (layer 2) and fills metrics. With
// a schema, extractJSONObject (fence/prose-tolerant, last-object-wins) → a parse
// failure is *agent.ErrUnparseableOutput (retryable). Without a schema, Output is
// nil. Turns is 1 (one call). Cost is DERIVED from tokens via the injected pricer
// (never the global pricing.Default() — that is set once at New). The engine
// re-validates Output against output_schema (ValidateOutputMap) — this adapter
// never imports engine.
//
// model is the wire model id captured from the streamed response (last non-empty
// across chunks); it is always stamped into Metrics.Model even on a pricing miss.
// An unknown or empty model → cost ABSENT (Source == ""), never $0.
// Cache normalization is PROVIDER-DEPENDENT (see metricsFrom): OpenAI/Gemini report a
// prompt-token count that INCLUDES cached tokens (subtract for cost); Anthropic reports
// input_tokens EXCLUSIVE of cache (no subtract). buildResult selects the mode from the
// resolved provider below.
func buildResult(full string, usage usageRec, model string, pricer pricing.Table, inv agent.AgentInvocation) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		obj, err := extractJSONObject(full)
		if err != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = obj
	}
	// Anthropic's input_tokens excludes cache tokens; the single-call path derives
	// the normalization mode from the resolved provider (callAnthropic doesn't call
	// metricsFrom itself).
	anthropicNorm := effectiveProvider(inv.With) == providerAnthropic
	ms := metricsFrom(usage, model, pricer, anthropicNorm)
	return agent.AgentResult{
		Output:   output,
		ExitCode: 0,
		Metrics:  ms,
		// Transcript.User is the CLEAN authored prompt (with["prompt"]), deliberately NOT the
		// assembled prompt the model saw (assemblePrompt adds the schema directive and, on a gate
		// repair attempt, the prior verdict). Rationale: a successor's thread should show the logical
		// turn, not repair/schema plumbing. Tradeoff: a gated continues-target that repaired threads a
		// user-half that omits the verdict which shaped the accepted answer — accepted as benign (the
		// alternative would pollute EVERY typed target's thread with the schema directive). Assistant
		// is the verbatim final message (prose, or the JSON object for a typed turn — D13).
		Transcript: agent.ThreadTurn{User: stringOr(inv.With, keyPrompt, ""), Assistant: full},
	}, nil
}

// metricsFrom builds a MetricSet from token usage + wire model, deriving USD cost
// via the injected pricer. anthropicNorm gates the cache normalization: OpenAI &
// Gemini report a prompt-token count that INCLUDES cached tokens (subtract them
// for the non-cached input cost); Anthropic reports input_tokens EXCLUSIVE of
// cache (do NOT subtract). Model "" → cost ABSENT (never $0).
func metricsFrom(usage usageRec, model string, pricer pricing.Table, anthropicNorm bool) agent.MetricSet {
	tokens := agent.MetricTokens{
		Input:              usage.Input,
		Output:             usage.Output,
		CacheReadInput:     usage.CacheRead,
		CacheCreationInput: usage.CacheWrite,
	}
	ms := agent.MetricSet{Tokens: tokens, Turns: 1, Model: model}
	if model != "" {
		inTokens := usage.Input
		if !anthropicNorm {
			inTokens -= usage.CacheRead // prompt count includes cached; normalize for cost
		}
		b := pricing.Breakdown{
			Input:      inTokens,
			Output:     usage.Output,
			CacheRead:  usage.CacheRead,
			CacheWrite: usage.CacheWrite,
		}
		if c, ok := pricer.Derive(model, b); ok {
			ms.Cost = agent.MetricCost{Source: agent.CostSourceDerived, Currency: c.Currency, Total: c.Total, Input: c.Input, Output: c.Output}
		}
	}
	return ms
}

// metricsFrom (method form) closes over the adapter's injected pricer — the
// tool-loop path's accessor. The tool-loop is OpenAI-compat only, so anthropicNorm
// is always false here.
func (a *Adapter) metricsFrom(usage usageRec, model string) agent.MetricSet {
	return metricsFrom(usage, model, a.pricer, false)
}

// tokenSummary is the DisplayFinal text (used by launch.go — B7).
func tokenSummary(u usageRec) string {
	return fmt.Sprintf("%d in / %d out tokens", u.Input, u.Output)
}

// extractJSONObject scans s for a JSON object, using json.Decoder
// (which consumes exactly one well-formed value), returning the LAST object that
// decodes (right bias for an agent that reasons before its final JSON). We do
// NOT schema-validate here (the engine's ValidateOutputMap does, so a
// wrong-but-valid object becomes a retryable schema failure) and do NOT pull in
// a json-repair dependency (it could fabricate a valid-but-wrong object).
func extractJSONObject(s string) (map[string]any, error) {
	s = stripJSONFence(strings.TrimSpace(s))
	var whole map[string]any
	if err := json.Unmarshal([]byte(s), &whole); err == nil {
		return whole, nil
	}
	var last map[string]any
	found := false
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var cand map[string]any
		if err := dec.Decode(&cand); err == nil {
			last = cand
			found = true
		}
	}
	if found {
		return last, nil
	}
	return nil, fmt.Errorf("agent/awfllm: no JSON object found in result")
}

// stripJSONFence removes a single leading ```json / ``` fence line and a
// trailing ``` if present.
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
