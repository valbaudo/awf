package codex

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

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
