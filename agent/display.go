package agent

// DisplayClass is the normalized, agent-agnostic category of a live AgentEvent.
// Each agent.Adapter maps its harness-specific event vocabulary onto these so a
// single renderer can present any agent's events without parsing its raw JSON.
// The zero value is DisplayOther (unclassified → terse fallback).
type DisplayClass uint8

const (
	DisplayOther      DisplayClass = iota // unknown/unclassified → terse fallback
	DisplayInit                           // session start (model, tools)
	DisplayAssistant                      // assistant narration (full)
	DisplayReasoning                      // chain-of-thought (dimmed, elided)
	DisplayToolCall                       // a tool invocation (one-line summary)
	DisplayToolResult                     // a tool's output (collapsed: status + size + head/tail)
	DisplayFinal                          // the final answer (full)
	DisplayError                          // an error (highlighted)
	DisplayNotice                         // transient notice (rate-limit/retry/status)
)

// EventDisplay is the adapter-populated, presentation-neutral summary the live
// renderer consumes. NEVER journaled (the field is json:"-"); the durable record
// is AgentEvent.Payload. The adapter fills only the fields relevant to Class.
//
// Text by Class: Assistant/Final → the full text; Reasoning → the reasoning text
// (renderer elides); ToolCall → a short arg summary; ToolResult → a bounded
// head+tail of the output (the adapter elides — the full body stays in Payload);
// Error/Notice/Init → the message/summary. Lines/Bytes are the FULL output
// counts for ToolResult.
//
// MUST stay comparable (only scalar fields) so AgentEvent remains ==-comparable.
type EventDisplay struct {
	Class   DisplayClass
	Tool    string // tool name (ToolCall/ToolResult)
	Text    string // see semantics above
	Lines   int    // ToolResult: full output line count (0 = n/a)
	Bytes   int    // ToolResult: full output byte size (0 = n/a)
	IsError bool   // ToolResult/Error: failure state
}
