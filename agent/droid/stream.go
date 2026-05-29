package droid

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// execEnvelope is droid's `-o json` result envelope (a single JSON object on
// stdout). Field set verified against droid v0.138.0.
type execEnvelope struct {
	Type       string    `json:"type"`    // "result"
	Subtype    string    `json:"subtype"` // "success" | "failure"
	IsError    bool      `json:"is_error"`
	DurationMS int64     `json:"duration_ms"`
	NumTurns   int       `json:"num_turns"`
	Result     string    `json:"result"`
	SessionID  string    `json:"session_id"`
	Usage      *usageRec `json:"usage"`
}

type usageRec struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ErrAuthFailureSentinel marks a droid result whose is_error is true and whose
// message names auth/FACTORY_API_KEY. Launch wraps it as *agent.ErrAgentLaunch
// (RETRYABLE): the envelope carries only free text, so we cannot distinguish a
// permanent bad key from a transient auth-infra error — the bounded retry
// budget covers the transient case, and the present-key precondition is checked
// in ValidateConfig.
var ErrAuthFailureSentinel = errors.New("agent/droid: droid exec reported is_error with an authentication failure")

// parseEnvelope decodes one `-o json` line. Wraps decode errors as *ErrStreamParse.
func parseEnvelope(b []byte) (*execEnvelope, error) {
	var env execEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, &ErrStreamParse{Line: b, Cause: err}
	}
	return &env, nil
}

// extractResult builds an AgentResult from a droid result envelope.
//   - is_error / subtype:"failure" → auth sentinel (if the message names auth)
//     or a generic failure; Launch maps both to ErrAgentLaunch (retryable).
//   - success + OutputSchema set → parse a JSON object out of Result;
//     non-parseable → *agent.ErrUnparseableOutput{NodePath} (retryable; the
//     engine then re-validates schema conformance via ValidateOutputMap).
//   - success + no schema → Output nil (matches claude; transcript lives in the
//     AgentEvent payload, not the typed-binding surface).
//
// Cost is left zero — droid's -o json reports no dollar figure.
func extractResult(env *execEnvelope, inv agent.AgentInvocation) (agent.AgentResult, error) {
	if env.IsError || env.Subtype == "failure" {
		if strings.Contains(env.Result, "Authentication failed") || strings.Contains(env.Result, "FACTORY_API_KEY") {
			return agent.AgentResult{}, fmt.Errorf("%w: %s", ErrAuthFailureSentinel, env.Result)
		}
		return agent.AgentResult{}, fmt.Errorf("agent/droid: droid exec failed (subtype %q): %s", env.Subtype, env.Result)
	}

	var output map[string]any
	if inv.OutputSchema != nil {
		parsed, perr := extractJSONObject(env.Result)
		if perr != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = parsed
	}

	var tokens agent.MetricTokens
	if env.Usage != nil {
		tokens.Input = env.Usage.InputTokens
		tokens.Output = env.Usage.OutputTokens
		tokens.CacheReadInput = env.Usage.CacheReadInputTokens
		tokens.CacheCreationInput = env.Usage.CacheCreationInputTokens
	}
	return agent.AgentResult{
		Output:   output,
		ExitCode: 0,
		Metrics:  agent.MetricSet{Tokens: tokens, Turns: env.NumTurns},
	}, nil
}

// extractJSONObject pulls a JSON object out of droid's free-text result. droid
// has no native schema mode, so Result may wrap the JSON in prose or a fence.
// Strategy (stdlib only, STRING-AWARE via json.Decoder so braces/quotes inside
// strings and escaped quotes don't fool it): strip a ```json fence → strict
// whole-text decode → else scan each '{' start, decode with json.Decoder (which
// consumes exactly one well-formed value), returning the LAST object that
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
	return nil, fmt.Errorf("agent/droid: no JSON object found in result")
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
