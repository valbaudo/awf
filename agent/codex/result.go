package codex

import (
	"encoding/json"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// buildResult builds the AgentResult from the LAST agent_message text and the
// terminal usage. With a schema, STRICT json.Unmarshal of the API-constrained text
// (no tolerant brace-scan — a tolerant scan would only hide a real schema
// regression); a parse failure → *agent.ErrUnparseableOutput. Without a schema,
// Output is nil. Cost zero (codex reports no USD).
func buildResult(finalText string, usage *usageRec, inv agent.AgentInvocation) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		if err := json.Unmarshal([]byte(strings.TrimSpace(finalText)), &output); err != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
	}
	var tokens agent.MetricTokens
	if usage != nil {
		// Pass codex's raw counts through, exactly as claude does (agent/claude/
		// stream.go does NOT subtract cache from Input either). NOTE: per the OpenAI
		// Responses usage schema, codex's input_tokens is INCLUSIVE of
		// cached_input_tokens (cached ⊂ input — verified: a cache-hit run reported
		// input 46736 / cached 38144). CacheReadInput is that cached subset. Do NOT
		// sum Input+CacheReadInput (it would double-count); obs (obs/attrs.go) emits
		// them as separate OTel attributes, so there is no wire double-count. We do
		// NOT subtract here — that would diverge from claude's pass-through and break
		// cross-adapter comparability.
		tokens.Input = usage.InputTokens
		tokens.Output = usage.OutputTokens
		tokens.CacheReadInput = usage.CachedInputTokens
	}
	// Metrics.Turns left 0: `codex exec` is single-turn and reports no num_turns in
	// --json usage (claude fills it from result.num_turns; codex has no equivalent).
	// Observability-only; not a contract field.
	return agent.AgentResult{Output: output, ExitCode: 0, Metrics: agent.MetricSet{Tokens: tokens}}, nil
}

// isPermanentCodexError parses a codex error message (a JSON-encoded API error)
// and reports whether it is a permanent client-side config fault (HTTP status 400
// AND error.type == invalid_request_error). A bare strings.Contains would
// false-positive on a 429/5xx body embedding the token, so we PARSE. Unparseable
// or non-matching → false → retryable (the safe default).
func isPermanentCodexError(message string) bool {
	var probe struct {
		Status int `json:"status"`
		Error  struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(message), &probe); err != nil {
		return false
	}
	return probe.Status == 400 && probe.Error.Type == "invalid_request_error"
}
