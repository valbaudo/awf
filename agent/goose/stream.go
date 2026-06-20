package goose

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/pricing"
)

// streamEvent is one line of goose's `--output-format stream-json` NDJSON. Three
// shapes: "message" (assistant text — an INCREMENTAL delta, not a snapshot),
// terminal "complete" (token totals), and "error" (a stream/provider fault).
type streamEvent struct {
	Type         string         `json:"type"`
	Message      *streamMessage `json:"message,omitempty"`
	InputTokens  int            `json:"input_tokens,omitempty"`
	OutputTokens int            `json:"output_tokens,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type streamMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseStreamEvent(b []byte) (*streamEvent, error) {
	var ev streamEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, &ErrStreamParse{Line: b, Cause: err}
	}
	return &ev, nil
}

// assistantText returns the concatenated text of a "message" event IFF it is
// role=="assistant" (else ""). goose streams assistant output as incremental
// deltas, so Launch concatenates assistantText across lines in arrival order. The
// role filter keeps tool/user echoes (e.g. the final_output continuation's user
// message) out of the accumulated final text, so a {...} in a non-assistant line
// can never be mistaken for the answer.
func assistantText(ev *streamEvent) string {
	if ev.Type != "message" || ev.Message == nil || ev.Message.Role != "assistant" {
		return ""
	}
	var b strings.Builder
	for _, blk := range ev.Message.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// buildResult builds the AgentResult from the fully-reassembled final assistant
// text and the terminal "complete" event. With a schema, extracts a JSON object
// (layer-2); non-parseable → *agent.ErrUnparseableOutput. Without a schema, Output
// is nil.
//
// Cost is DERIVED from tokens via pricer (the adapter's injected pricing.Table —
// never the global pricing.Default() here, so tests can swap a fixture table).
// goose's harness emits no model id, so we price on the REQUESTED with:{model}:
// unset/empty model OR a model the table doesn't know → cost ABSENT (Source==""),
// which is correct, not $0. goose reports no cache fields, so Input is passed to
// the pricing package as-is (no cache subset to subtract).
func buildResult(finalText string, complete *streamEvent, inv agent.AgentInvocation, pricer pricing.Table) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		parsed, perr := extractJSONObject(finalText)
		if perr != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = parsed
	}
	var tokens agent.MetricTokens
	if complete != nil {
		tokens.Input = complete.InputTokens
		tokens.Output = complete.OutputTokens
	}
	model, _ := inv.With[keyModel].(string)
	ms := agent.MetricSet{Tokens: tokens, Model: model}
	if model != "" {
		b := pricing.Breakdown{Input: tokens.Input, Output: tokens.Output} // goose has no cache subset
		if c, ok := pricer.Derive(model, b); ok {
			ms.Cost = agent.MetricCost{Source: agent.CostSourceDerived, Currency: c.Currency, Total: c.Total, Input: c.Input, Output: c.Output}
		}
	}
	return agent.AgentResult{Output: output, ExitCode: 0, Metrics: ms}, nil
}

// extractJSONObject pulls a JSON object out of goose's free-text result. goose
// has no native schema mode, so the final text may wrap the JSON in prose or a
// fence. Strategy (stdlib only, STRING-AWARE via json.Decoder so braces/quotes
// inside strings and escaped quotes don't fool it): strip a ```json fence →
// strict whole-text decode → else scan each '{' start, decode with json.Decoder
// (which consumes exactly one well-formed value), returning the LAST object that
// decodes (right bias for an agent that reasons before its final JSON). We do
// NOT schema-validate here (the engine's ValidateOutputMap does, so a
// wrong-but-valid object becomes a retryable schema failure) and do NOT pull in
// a json-repair dependency (it could fabricate a valid-but-wrong object).
func extractJSONObject(s string) (map[string]any, error) {
	s = stripJSONFence(agent.StripThinkTags(strings.TrimSpace(s)))
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

// displayForGoose maps one goose stream-json event to agent.EventDisplay.
func displayForGoose(ev *streamEvent) agent.EventDisplay {
	switch ev.Type {
	case "message":
		if t := assistantText(ev); t != "" {
			return agent.EventDisplay{Class: agent.DisplayAssistant, Text: t}
		}
		return agent.EventDisplay{} // non-assistant message → Other (terse)
	case "complete":
		return agent.EventDisplay{Class: agent.DisplayFinal, Text: fmt.Sprintf("%d in / %d out tokens", ev.InputTokens, ev.OutputTokens)}
	case "error":
		return agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: ev.Error}
	default:
		return agent.EventDisplay{}
	}
}
