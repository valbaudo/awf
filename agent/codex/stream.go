package codex

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
