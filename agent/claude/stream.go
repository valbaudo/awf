package claude

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// streamMessage mirrors the discriminated-union Phase 5 design Appendix A
// pins. Empirically verified against claude 2.1.153 stream-json output.
type streamMessage struct {
	Type    string `json:"type"`              // "system" | "assistant" | "user" | "result" | "rate_limit_event"
	Subtype string `json:"subtype,omitempty"` // system: "init" | "hook_started" | "hook_response"; result: "success" | "error_max_structured_output_retries"

	// system/init fields
	SessionID      string          `json:"session_id,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	Model          string          `json:"model,omitempty"`
	Tools          []string        `json:"tools,omitempty"`
	MCPServers     json.RawMessage `json:"mcp_servers,omitempty"`
	Version        string          `json:"claude_code_version,omitempty"`
	APIKeySource   string          `json:"apiKeySource,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	OutputStyle    string          `json:"output_style,omitempty"`
	Agents         json.RawMessage `json:"agents,omitempty"`
	Skills         json.RawMessage `json:"skills,omitempty"`
	Plugins        json.RawMessage `json:"plugins,omitempty"`
	UUID           string          `json:"uuid,omitempty"`

	// assistant/user fields
	Message json.RawMessage `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`

	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info,omitempty"`

	// result fields
	IsError          bool            `json:"is_error,omitempty"`
	APIErrorStatus   json.RawMessage `json:"api_error_status,omitempty"`
	DurationMS       int64           `json:"duration_ms,omitempty"`
	DurationAPIMS    int64           `json:"duration_api_ms,omitempty"`
	TTFTMS           int64           `json:"ttft_ms,omitempty"`
	NumTurns         int             `json:"num_turns,omitempty"`
	Result           string          `json:"result,omitempty"`
	StopReason       string          `json:"stop_reason,omitempty"`
	TotalCostUSD     float64         `json:"total_cost_usd,omitempty"`
	Usage            *usageRec       `json:"usage,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	TerminalReason   string          `json:"terminal_reason,omitempty"`
	FastModeState    string          `json:"fast_mode_state,omitempty"`
}

type usageRec struct {
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens"`
	CacheCreation            json.RawMessage `json:"cache_creation,omitempty"`
	ServiceTier              string          `json:"service_tier,omitempty"`
}

type rateLimitInfo struct {
	Status             string  `json:"status"`
	ResetsAt           int64   `json:"resetsAt"`
	RateLimitType      string  `json:"rateLimitType"`
	Utilization        float64 `json:"utilization"`
	IsUsingOverage     bool    `json:"isUsingOverage"`
	SurpassedThreshold float64 `json:"surpassedThreshold"`
}

// messageContentBlock is the per-block shape inside an assistant message.
// Used to split multi-block assistant messages into one event per block per
// Phase 5 design decision 14. Only fields actually consumed by
// splitAssistantMessage; forward-compat fields are out of scope (rule 2).
type messageContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// parseStreamLine decodes one stream-json line. Wraps json.Unmarshal errors
// as *ErrStreamParse for the operator-facing path.
func parseStreamLine(b []byte) (streamMessage, error) {
	var msg streamMessage
	if err := json.Unmarshal(b, &msg); err != nil {
		return streamMessage{}, &ErrStreamParse{Line: b, Cause: err}
	}
	return msg, nil
}

// messageToEvents splits one streamMessage into one or more AgentEvents.
// Per Phase 5 design decision 14: assistant messages split per content
// block (one event per text / thinking / tool_use / tool_result).
// System / rate_limit / user / result emit a single event each.
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
		// v1 clean stub: claude delivers tool RESULTS inside user messages with
		// polymorphic content (string OR []{text}); rather than spew raw JSON, show
		// a terse notice. Full per-result rendering is a fast-follow.
		return []agent.AgentEvent{{Kind: "user", Payload: raw, Stream: "stdout",
			Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: "tool result"}}}
	case "assistant":
		return splitAssistantMessage(msg)
	default:
		raw, _ := json.Marshal(msg)
		return []agent.AgentEvent{{Kind: msg.Type, Payload: raw, Stream: "stdout"}}
	}
}

// splitAssistantMessage decodes the embedded Message and emits one
// AgentEvent per content block.
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
		out = append(out, agent.AgentEvent{Kind: block.Type, Payload: payload, Stream: "stdout", Display: displayForClaudeBlock(block)})
	}
	return out
}

// displayForClaudeBlock maps one assistant content block to the normalized
// EventDisplay. Unknown block types (e.g. tool_result inside assistant) return
// the zero value → DisplayOther. The tool_use arg is summarized via the shared
// agent.SummarizeToolInput (one bound, one salient-key list across adapters).
func displayForClaudeBlock(b messageContentBlock) agent.EventDisplay {
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

// ErrNoResultEvent is the sentinel returned by extractResult when the
// streamMessage isn't a result event. Internal — Launch checks for it.
var ErrNoResultEvent = errors.New("agent/claude: streamMessage is not a result event")

// ErrAuthFailureSentinel is the sentinel returned by extractResult when the
// result event has subtype:"success" but is_error:true (auth failure path —
// verified against real claude 2.1.153). Launch wraps this as
// *agent.ErrAgentLaunch so the engine maps to retryable_failure (or
// permanent_failure via the adapter contract if the message text is "Not
// logged in").
var ErrAuthFailureSentinel = errors.New("agent/claude: result event has is_error:true")

// extractResult builds an AgentResult from a result-typed streamMessage.
// Returns ErrNoResultEvent if the message type is not "result"; returns a
// non-nil error carrying "structured_output" verbiage for the
// error_max_structured_output_retries subtype (Launch maps to
// *agent.ErrUnparseableOutput); returns nil error + populated AgentResult
// for the success subtype (and is_error: false).
//
// Auth failures: real claude returns {"subtype":"success", "is_error":true,
// "result":"Not logged in"} WITHOUT a structured_output field. Pre-fix
// extractResult would have returned AgentResult{Output: nil} with nil
// error, silently masking the auth failure as a schema-violation retry.
// We check is_error FIRST inside the success case and return a wrapped
// error carrying the result text so Launch can produce *agent.ErrAgentLaunch.
//
// model is the value captured from the system/init event's "model" field.
// It is stored in Metrics.Model for auditability. The result event itself
// does not carry the model — the caller must thread it from the init event.
func extractResult(msg streamMessage, model string) (agent.AgentResult, error) {
	if msg.Type != "result" {
		return agent.AgentResult{}, ErrNoResultEvent
	}
	switch msg.Subtype {
	case "success":
		// Check is_error BEFORE treating subtype:success as happy path.
		if msg.IsError {
			return agent.AgentResult{}, fmt.Errorf("%w: %s", ErrAuthFailureSentinel, msg.Result)
		}
		var output map[string]any
		if len(msg.StructuredOutput) > 0 {
			if err := json.Unmarshal(msg.StructuredOutput, &output); err != nil {
				return agent.AgentResult{}, fmt.Errorf("agent/claude: unmarshal structured_output: %w", err)
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
		return agent.AgentResult{}, fmt.Errorf("agent/claude: claude returned error_max_structured_output_retries — structured_output retry budget exhausted")
	default:
		return agent.AgentResult{}, fmt.Errorf("agent/claude: unexpected result subtype %q", msg.Subtype)
	}
}
