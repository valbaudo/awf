package codex_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
)

func TestDisplayForCodex_Classes(t *testing.T) {
	asst, _ := codex.ParseStreamEventForTest(itemMsg("hello"))
	if d := codex.DisplayForCodexForTest(asst); d.Class != agent.DisplayAssistant || d.Text != "hello" {
		t.Errorf("agent_message display = %+v", d)
	}
	// command_execution ToolCall Text MUST be non-empty for a bare-string command —
	// guards against using SummarizeToolInput (which expects an object → returns "").
	cs, _ := codex.ParseStreamEventForTest(cmdStarted("cat note.txt"))
	if d := codex.DisplayForCodexForTest(cs); d.Class != agent.DisplayToolCall || d.Text == "" {
		t.Errorf("command_execution ToolCall display = %+v, want DisplayToolCall with non-empty Text", d)
	}
	cc, _ := codex.ParseStreamEventForTest(cmdCompleted("cat note.txt", "hello world\n", 1))
	if d := codex.DisplayForCodexForTest(cc); d.Class != agent.DisplayToolResult || !d.IsError {
		t.Errorf("command_execution (exit 1) display = %+v, want DisplayToolResult IsError", d)
	}
	done, _ := codex.ParseStreamEventForTest(turnCompleted(1, 0, 1))
	if d := codex.DisplayForCodexForTest(done); d.Class != agent.DisplayFinal {
		t.Errorf("turn.completed display class = %v, want DisplayFinal", d.Class)
	}
	tf, _ := codex.ParseStreamEventForTest(turnFailed(apiErr400))
	if d := codex.DisplayForCodexForTest(tf); d.Class != agent.DisplayError || !d.IsError {
		t.Errorf("turn.failed display = %+v, want DisplayError IsError", d)
	}
}
