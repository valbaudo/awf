package agent

import "github.com/valbaudo/awf/ir"

// ToolCall is one model-emitted tool invocation. Arguments is the RAW model-emitted
// JSON string, stored verbatim (the §4.5 determinism invariant — never reserialized).
type ToolCall struct {
	Index     int    `json:"index"` // stable position; the J in react[N].round-K.tool-J
	ID        string `json:"id"`    // matches the tool-role message's tool_call_id
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ReactTurn is one message in an engine-owned tool-loop conversation. Role is
// "user" | "assistant" | "tool". An assistant turn may carry ToolCalls; a tool turn
// carries ToolCallID + Content (the result). Distinct from ThreadTurn (continues:)
// which cannot represent tool_calls / tool-role messages.
type ReactTurn struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant turns only; OMIT (not []) when none
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool turns only
}

// ToolDef is a tool offered to the model (name + description + parameters schema).
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolLoopInvocation is ONE model call with tools attached + the full prior message
// history. The engine (runReact) owns the history; the adapter just executes the call.
// NOTE: no Env field — the awf/llm key rides a.env at adapter construction (config.go),
// never the invocation (rev #17).
type ToolLoopInvocation struct {
	NodePath     string         `json:"node_path"`
	Uses         string         `json:"uses"`
	With         ir.RawConfig   `json:"with,omitempty"`
	Messages     []ReactTurn    `json:"messages"`
	Tools        []ToolDef      `json:"tools"`
	OutputSchema *ir.JSONSchema `json:"output_schema,omitempty"` // steers response_format (§6); engine validates post-hoc
}

// ToolLoopResult is the model's response for one call. Output is the PARSED final
// answer (rev #4): on a natural-stop round with an output_schema, RunToolLoop parses
// the assistant text into Output via the adapter's own extractJSONObject and returns
// *ErrUnparseableOutput on a miss — so the ENGINE validates Output with
// engine.ValidateOutputMap WITHOUT importing agent/awfllm. Output is nil on tool_calls
// rounds, on max_turns truncation, and when no output_schema is declared.
type ToolLoopResult struct {
	Text         string         `json:"text"`
	Output       map[string]any `json:"output,omitempty"`
	ToolCalls    []ToolCall     `json:"tool_calls,omitempty"`
	FinishReason string         `json:"finish_reason"`
	Metrics      *MetricSet     `json:"metrics,omitempty"`
}
