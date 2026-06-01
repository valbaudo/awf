package droid

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
)

type usageRec struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ErrAuthFailureSentinel marks a droid terminal "error" event whose message
// names auth/FACTORY_API_KEY. Launch wraps it as *agent.ErrAgentLaunch
// (RETRYABLE): the event carries only free text, so we cannot distinguish a
// permanent bad key from a transient auth-infra error — the bounded retry
// budget covers the transient case, and the present-key precondition is checked
// in ValidateConfig.
var ErrAuthFailureSentinel = errors.New("agent/droid: droid exec reported an authentication failure")

// streamEvent is one line of droid's `-o stream-json` NDJSON output. droid emits
// these incrementally as the agent works (verified v0.138.0): a "system"/init
// line, "message" lines, "tool_call"/"tool_result" lines, then a terminal
// "completion" (success) — or a terminal "error" (failure, e.g. auth). Only the
// fields the adapter needs for the OUTCOME are modeled; live AgentEvents carry
// the raw line, so per-event detail isn't decoded here.
type streamEvent struct {
	Type string `json:"type"` // system | message | reasoning | tool_call | tool_result | completion | error | status

	// completion (terminal, success). Note: camelCase in the wire format.
	FinalText  string    `json:"finalText,omitempty"`
	NumTurns   int       `json:"numTurns,omitempty"`
	DurationMS int64     `json:"durationMs,omitempty"`
	Usage      *usageRec `json:"usage,omitempty"`

	// error (terminal, failure)
	Source  string `json:"source,omitempty"`
	Message string `json:"message,omitempty"`

	// display-only fields (verified against droid v0.138.0 -o stream-json)
	Role       string          `json:"role,omitempty"`
	Text       string          `json:"text,omitempty"`
	Model      string          `json:"model,omitempty"`
	Tools      []string        `json:"tools,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	ToolID     string          `json:"toolId,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Value      string          `json:"value,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Details    string          `json:"details,omitempty"`
}

// parseStreamEvent decodes one stream-json line. Wraps decode errors as *ErrStreamParse.
func parseStreamEvent(b []byte) (*streamEvent, error) {
	var ev streamEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, &ErrStreamParse{Line: b, Cause: err}
	}
	return &ev, nil
}

// resultFromCompletion builds an AgentResult from the terminal "completion"
// event. With a schema, parses a JSON object out of finalText (layer-2);
// non-parseable → *agent.ErrUnparseableOutput{NodePath}. Without a schema,
// Output is nil (the transcript lives in the streamed AgentEvents). Cost zero
// (droid reports no USD).
func resultFromCompletion(ev *streamEvent, inv agent.AgentInvocation) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		parsed, perr := extractJSONObject(ev.FinalText)
		if perr != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = parsed
	}
	var tokens agent.MetricTokens
	if ev.Usage != nil {
		tokens.Input = ev.Usage.InputTokens
		tokens.Output = ev.Usage.OutputTokens
		tokens.CacheReadInput = ev.Usage.CacheReadInputTokens
		tokens.CacheCreationInput = ev.Usage.CacheCreationInputTokens
	}
	return agent.AgentResult{Output: output, ExitCode: 0, Metrics: agent.MetricSet{Tokens: tokens, Turns: ev.NumTurns}}, nil
}

// errorFromEvent maps a terminal "error" event to an outcome error. Auth
// failures (message names auth / FACTORY_API_KEY) wrap ErrAuthFailureSentinel;
// Launch maps both to retryable *agent.ErrAgentLaunch (the message is free text,
// so a bad key can't be told from a transient auth-infra fault — bounded retry
// covers the transient case; the present-key precondition is in ValidateConfig).
func errorFromEvent(ev *streamEvent) error {
	if strings.Contains(ev.Message, "Authentication failed") || strings.Contains(ev.Message, "FACTORY_API_KEY") {
		return fmt.Errorf("%w: %s", ErrAuthFailureSentinel, ev.Message)
	}
	return fmt.Errorf("agent/droid: droid exec error (%s): %s", ev.Source, ev.Message)
}

// extractJSONObject pulls a JSON object out of droid's free-text result. droid
// has no native schema mode, so finalText may wrap the JSON in prose or a fence.
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

// displayForDroid maps one droid stream-json event to the normalized
// agent.EventDisplay. droid is the only place that understands droid's JSON;
// unknown types return the zero value (DisplayOther → terse fallback).
func displayForDroid(ev *streamEvent) agent.EventDisplay {
	switch ev.Type {
	case "system":
		return agent.EventDisplay{Class: agent.DisplayInit, Text: fmt.Sprintf("%s · %d tools", ev.Model, len(ev.Tools))}
	case "message":
		if ev.Role == "assistant" {
			return agent.EventDisplay{Class: agent.DisplayAssistant, Text: ev.Text}
		}
		return agent.EventDisplay{} // user echo → Other (terse)
	case "reasoning":
		return agent.EventDisplay{Class: agent.DisplayReasoning, Text: ev.Text}
	case "tool_call":
		return agent.EventDisplay{Class: agent.DisplayToolCall, Tool: ev.ToolName, Text: agent.SummarizeToolInput(ev.Parameters)}
	case "tool_result":
		return agent.EventDisplay{
			Class: agent.DisplayToolResult, Tool: ev.ToolID, IsError: ev.IsError,
			Text:  agent.Elide(ev.Value, agent.ToolResultHeadTail, agent.ToolResultHeadTail),
			Lines: agent.CountLines(ev.Value), Bytes: len(ev.Value),
		}
	case "completion":
		return agent.EventDisplay{Class: agent.DisplayFinal, Text: ev.FinalText}
	case "error":
		return agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: ev.Message}
	case "status":
		t := ev.Text
		if t == "" {
			t = ev.Details
		}
		return agent.EventDisplay{Class: agent.DisplayNotice, Text: t}
	default:
		return agent.EventDisplay{}
	}
}
