package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// streamEvent is one line of codex `exec --json` JSONL. Discriminated on Type:
// thread.started | turn.started | item.started | item.completed | turn.completed |
// turn.failed | error. Unknown types are tolerated (fall to DisplayOther / no-op).
type streamEvent struct {
	Type     string        `json:"type"`
	ThreadID string        `json:"thread_id,omitempty"`
	Item     *streamItem   `json:"item,omitempty"`
	Usage    *usageRec     `json:"usage,omitempty"`   // turn.completed
	Message  string        `json:"message,omitempty"` // "error" event: a JSON-encoded API error string
	Error    *turnErrorRec `json:"error,omitempty"`   // "turn.failed": {"message": <same JSON-encoded string>}
}

// streamItem is the `item` payload of item.started / item.completed. item.type is
// agent_message | command_execution | reasoning | ... ExitCode is a pointer so a
// missing field (item.started) is distinguishable from exit 0 (item.completed).
type streamItem struct {
	ID               string `json:"id,omitempty"`
	Type             string `json:"type,omitempty"`
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
}

// usageRec mirrors codex's turn.completed usage block. reasoning_output_tokens is
// a SUBSET of output_tokens (not mapped, to avoid double-count). cached_input_tokens
// maps to MetricTokens.CacheReadInput.
type usageRec struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

type turnErrorRec struct {
	Message string `json:"message"`
}

func parseStreamEvent(b []byte) (*streamEvent, error) {
	var ev streamEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, &ErrStreamParse{Line: b, Cause: err}
	}
	return &ev, nil
}

// eventKind is the descriptive Kind for an AgentEvent: item.type for item.* events
// (e.g. "agent_message", "command_execution", "reasoning"), else the top-level type.
func eventKind(ev *streamEvent) string {
	if (ev.Type == "item.started" || ev.Type == "item.completed") && ev.Item != nil && ev.Item.Type != "" {
		return ev.Item.Type
	}
	return ev.Type
}

// agentMessageText returns the item text IFF ev is an item.completed carrying an
// agent_message; else ("", false). Launch records this last-wins (codex may emit
// multiple agent_message items per turn — a premature one before a tool call,
// then the final answer).
func agentMessageText(ev *streamEvent) (string, bool) {
	if ev.Type != "item.completed" || ev.Item == nil || ev.Item.Type != "agent_message" {
		return "", false
	}
	return ev.Item.Text, true
}

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

// displayForCodex maps one codex stream event to agent.EventDisplay. The
// command_execution Text uses agent.Elide (a STRING helper) — NOT
// SummarizeToolInput, which expects a JSON object and returns "" for codex's
// bare-string command field.
func displayForCodex(ev *streamEvent) agent.EventDisplay {
	switch ev.Type {
	case "thread.started":
		return agent.EventDisplay{Class: agent.DisplayInit, Text: ev.ThreadID}
	case "item.started", "item.completed":
		if ev.Item == nil {
			return agent.EventDisplay{}
		}
		switch ev.Item.Type {
		case "agent_message":
			return agent.EventDisplay{Class: agent.DisplayAssistant, Text: ev.Item.Text}
		case "reasoning":
			return agent.EventDisplay{Class: agent.DisplayReasoning, Text: ev.Item.Text}
		case "command_execution":
			if ev.Type == "item.started" {
				return agent.EventDisplay{
					Class: agent.DisplayToolCall, Tool: "shell",
					Text: agent.Elide(ev.Item.Command, agent.ToolResultHeadTail, agent.ToolResultHeadTail),
				}
			}
			return agent.EventDisplay{
				Class: agent.DisplayToolResult, Tool: "shell",
				Text:    agent.Elide(ev.Item.AggregatedOutput, agent.ToolResultHeadTail, agent.ToolResultHeadTail),
				Lines:   agent.CountLines(ev.Item.AggregatedOutput),
				Bytes:   len(ev.Item.AggregatedOutput),
				IsError: ev.Item.ExitCode != nil && *ev.Item.ExitCode != 0,
			}
		default:
			return agent.EventDisplay{}
		}
	case "turn.completed":
		var in, out int
		if ev.Usage != nil {
			in, out = ev.Usage.InputTokens, ev.Usage.OutputTokens
		}
		return agent.EventDisplay{Class: agent.DisplayFinal, Text: fmt.Sprintf("%d in / %d out tokens", in, out)}
	case "error":
		return agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: ev.Message}
	case "turn.failed":
		msg := ""
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		return agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: msg}
	default:
		return agent.EventDisplay{}
	}
}
