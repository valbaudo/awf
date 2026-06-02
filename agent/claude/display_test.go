package claude

import (
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestDisplay_System(t *testing.T) {
	evs := messageToEvents(streamMessage{Type: "system", Model: "claude-opus-4-8", Tools: []string{"Bash", "Read"}})
	if evs[0].Display.Class != agent.DisplayInit || evs[0].Display.Text == "" {
		t.Fatalf("system: %+v", evs[0].Display)
	}
}

func TestDisplay_AssistantBlocks(t *testing.T) {
	msg := streamMessage{Type: "assistant", Message: []byte(`{"content":[{"type":"text","text":"hello"},{"type":"thinking","thinking":"hmm"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}`)}
	evs := messageToEvents(msg)
	if len(evs) != 3 {
		t.Fatalf("want 3 block events, got %d", len(evs))
	}
	if evs[0].Display.Class != agent.DisplayAssistant || evs[0].Display.Text != "hello" {
		t.Errorf("text: %+v", evs[0].Display)
	}
	if evs[1].Display.Class != agent.DisplayReasoning || evs[1].Display.Text != "hmm" {
		t.Errorf("thinking: %+v", evs[1].Display)
	}
	if evs[2].Display.Class != agent.DisplayToolCall || evs[2].Display.Tool != "Bash" || evs[2].Display.Text != "ls" {
		t.Errorf("tool_use: %+v", evs[2].Display)
	}
}

func TestDisplay_ResultAndRateLimitAndUser(t *testing.T) {
	if messageToEvents(streamMessage{Type: "result", Subtype: "success", Result: "done"})[0].Display.Class != agent.DisplayFinal {
		t.Error("result → Final")
	}
	if messageToEvents(streamMessage{Type: "rate_limit_event"})[0].Display.Class != agent.DisplayNotice {
		t.Error("rate_limit → Notice")
	}
	u := messageToEvents(streamMessage{Type: "user", Message: []byte(`{"content":[{"type":"tool_result"}]}`)})[0]
	if u.Display.Class != agent.DisplayNotice || u.Display.Text == "" {
		t.Errorf("user → Notice stub, got %+v", u.Display)
	}
}
