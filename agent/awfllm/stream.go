package awfllm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// usageRec is the normalized token usage both transports fill (OpenAI
// prompt/completion + cached; Ollama prompt_eval/eval). CacheRead maps to
// MetricTokens.CacheReadInput. Zero when a backend omits usage (obs-only).
type usageRec struct {
	Input     int
	Output    int
	CacheRead int
}

// apiError is the uniform HTTP error both transports produce, so classification
// is wire-shaped (status + type), not SDK-symbol-shaped. The OpenAI path maps
// *openai.Error → apiError; the Ollama path maps a non-2xx response → apiError.
type apiError struct {
	Status int
	Type   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("agent/awfllm: HTTP %d (%s): %s", e.Status, e.Type, e.Body)
}

// isPermanentLLMError reports a permanent client-side config fault: HTTP 400 AND
// type invalid_request_error (bad model, or a schema the backend rejects under
// strict mode). Everything else — including non-apiError transport failures —
// is retryable (the safe default; the gate/retry budget bounds attempts).
func isPermanentLLMError(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == 400 && ae.Type == "invalid_request_error"
	}
	return false
}

// buildResult parses the reassembled full text (layer 2) and fills metrics. With
// a schema, extractJSONObject (fence/prose-tolerant, last-object-wins) → a parse
// failure is *agent.ErrUnparseableOutput (retryable). Without a schema, Output is
// nil. Cost is 0 (no pricing pkg); Turns is 1 (one call). The engine re-validates
// Output against output_schema (ValidateOutputMap) — this adapter never imports engine.
func buildResult(full string, usage usageRec, inv agent.AgentInvocation) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		obj, err := extractJSONObject(full)
		if err != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = obj
	}
	return agent.AgentResult{
		Output:   output,
		ExitCode: 0,
		Metrics: agent.MetricSet{
			Tokens: agent.MetricTokens{Input: usage.Input, Output: usage.Output, CacheReadInput: usage.CacheRead},
			Turns:  1,
		},
	}, nil
}

// tokenSummary is the DisplayFinal text (used by launch.go — B7).
//
//nolint:unused
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
	return nil, fmt.Errorf("agent/goose: no JSON object found in result")
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
