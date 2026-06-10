package codex

import "encoding/json"

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
