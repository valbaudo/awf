package claudesession

// Stream parsing for Claude Code's --output-format stream-json output.
//
// This is a faithful copy of the parsing logic in agent/claude/stream.go,
// adjusted for the claudesession package namespace. It is deliberately
// duplicated rather than exported from agent/claude because:
//   1. The claude package's stream types are internal to that package
//      (unexported) per the codebase's modularity discipline.
//   2. Exporting would mean importing claudesession → claude which creates
//      a tighter coupling than needed.
//   3. The parsing contract is stable (empirically pinned against claude
//      2.1.153 stream-json format); the few lines of divergence are
//      localised error-prefix strings ("agent/claudesession:" vs "agent/claude:").
//
// If the upstream stream format changes, update BOTH packages.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// streamMessage mirrors the discriminated-union output of claude's
// --output-format stream-json. Empirically pinned against claude 2.1.153.
type streamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`

	// system/init
	SessionID string   `json:"session_id,omitempty"`
	Model     string   `json:"model,omitempty"`
	Tools     []string `json:"tools,omitempty"`

	// assistant/user
	Message json.RawMessage `json:"message,omitempty"`

	// result
	IsError          bool            `json:"is_error,omitempty"`
	NumTurns         int             `json:"num_turns,omitempty"`
	Result           string          `json:"result,omitempty"`
	TotalCostUSD     float64         `json:"total_cost_usd,omitempty"`
	Usage            *usageRec       `json:"usage,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

type usageRec struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type messageContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// errStreamParse is the parse failure sentinel (internal to this package).
type errStreamParse struct {
	Line  []byte
	Cause error
}

func (e *errStreamParse) Error() string {
	const maxLine = 200
	preview := e.Line
	if len(preview) > maxLine {
		preview = preview[:maxLine]
	}
	return fmt.Sprintf("agent/claudesession: parse stream-json line %q: %v", preview, e.Cause)
}
func (e *errStreamParse) Unwrap() error { return e.Cause }

// errUnexpectedExit is used when claude exits without emitting a result event.
type errUnexpectedExit struct {
	ExitCode int
}

func (e *errUnexpectedExit) Error() string {
	return fmt.Sprintf("agent/claudesession: claude exited with code %d before emitting a result event", e.ExitCode)
}

// errAuthFailureSentinel signals a result event with is_error:true.
var errAuthFailureSentinel = errors.New("agent/claudesession: result event has is_error:true")

func parseStreamLine(b []byte) (streamMessage, error) {
	var msg streamMessage
	if err := json.Unmarshal(b, &msg); err != nil {
		return streamMessage{}, &errStreamParse{Line: b, Cause: err}
	}
	return msg, nil
}

func messageToEvents(msg streamMessage) []agent.AgentEvent {
	switch msg.Type {
	case "system":
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: "system", Payload: raw, Stream: "stdout",
			Display: agent.EventDisplay{Class: agent.DisplayInit, Text: fmt.Sprintf("%s · %d tools", msg.Model, len(msg.Tools))}}}
	case "rate_limit_event":
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: "rate_limit", Payload: raw, Stream: "stdout",
			Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: "rate limit"}}}
	case "result":
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: "result", Payload: raw, Stream: "stdout",
			Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: msg.Result}}}
	case "user":
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: "user", Payload: raw, Stream: "stdout",
			Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: "tool result"}}}
	case "assistant":
		return splitAssistantMessage(msg)
	default:
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: msg.Type, Payload: raw, Stream: "stdout"}}
	}
}

func splitAssistantMessage(msg streamMessage) []agent.AgentEvent {
	var wrapper struct {
		Content []messageContentBlock `json:"content"`
	}
	if err := json.Unmarshal(msg.Message, &wrapper); err != nil {
		return []agent.AgentEvent{{Kind: "assistant", Payload: msg.Message, Stream: "stdout"}}
	}
	if len(wrapper.Content) == 0 {
		return []agent.AgentEvent{{Kind: "assistant", Payload: msg.Message, Stream: "stdout"}}
	}
	out := make([]agent.AgentEvent, 0, len(wrapper.Content))
	for _, block := range wrapper.Content {
		payload, _ := json.Marshal(block)
		out = append(out, agent.AgentEvent{Kind: block.Type, Payload: payload, Stream: "stdout", Display: displayForBlock(block)})
	}
	return out
}

func displayForBlock(b messageContentBlock) agent.EventDisplay {
	switch b.Type {
	case "text":
		return agent.EventDisplay{Class: agent.DisplayAssistant, Text: b.Text}
	case "thinking":
		return agent.EventDisplay{Class: agent.DisplayReasoning, Text: b.Thinking}
	case "tool_use":
		return agent.EventDisplay{Class: agent.DisplayToolCall, Tool: b.Name, Text: agent.SummarizeToolInput(b.Input)}
	default:
		return agent.EventDisplay{}
	}
}

func extractResult(msg streamMessage, model string) (agent.AgentResult, error) {
	if msg.Type != "result" {
		return agent.AgentResult{}, errors.New("agent/claudesession: streamMessage is not a result event")
	}
	switch msg.Subtype {
	case "success":
		if msg.IsError {
			return agent.AgentResult{}, fmt.Errorf("%w: %s", errAuthFailureSentinel, msg.Result)
		}
		var output map[string]any
		if len(msg.StructuredOutput) > 0 {
			if err := json.Unmarshal(msg.StructuredOutput, &output); err != nil {
				return agent.AgentResult{}, fmt.Errorf("agent/claudesession: unmarshal structured_output: %w", err)
			}
		}
		var tokens agent.MetricTokens
		if msg.Usage != nil {
			tokens.Input = msg.Usage.InputTokens
			tokens.Output = msg.Usage.OutputTokens
			tokens.CacheCreationInput = msg.Usage.CacheCreationInputTokens
			tokens.CacheReadInput = msg.Usage.CacheReadInputTokens
		}
		return agent.AgentResult{
			Output:   output,
			ExitCode: 0,
			Metrics: agent.MetricSet{
				Cost:   agent.MetricCost{Total: msg.TotalCostUSD, Source: agent.CostSourceReported},
				Tokens: tokens,
				Turns:  msg.NumTurns,
				Model:  model,
			},
		}, nil
	case "error_max_structured_output_retries":
		return agent.AgentResult{}, fmt.Errorf("agent/claudesession: claude returned error_max_structured_output_retries — structured_output retry budget exhausted")
	default:
		return agent.AgentResult{}, fmt.Errorf("agent/claudesession: unexpected result subtype %q", msg.Subtype)
	}
}
