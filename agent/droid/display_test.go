package droid

import (
	"testing"

	"github.com/valbaudo/awf/agent"
)

func disp(t *testing.T, line string) agent.EventDisplay {
	t.Helper()
	ev, err := parseStreamEvent([]byte(line))
	if err != nil {
		t.Fatalf("parseStreamEvent(%s): %v", line, err)
	}
	return displayForDroid(ev)
}

func TestDisplayForDroid_Init(t *testing.T) {
	d := disp(t, `{"type":"system","subtype":"init","model":"claude-opus-4-8","tools":["Read","LS","Execute"]}`)
	if d.Class != agent.DisplayInit || d.Text == "" {
		t.Fatalf("got %+v, want Init with text", d)
	}
}

func TestDisplayForDroid_AssistantAndUser(t *testing.T) {
	a := disp(t, `{"type":"message","role":"assistant","text":"here is the answer"}`)
	if a.Class != agent.DisplayAssistant || a.Text != "here is the answer" {
		t.Errorf("assistant: got %+v", a)
	}
	if disp(t, `{"type":"message","role":"user","text":"the prompt"}`).Class != agent.DisplayOther {
		t.Errorf("user echo should be Other (terse)")
	}
}

func TestDisplayForDroid_Reasoning(t *testing.T) {
	if d := disp(t, `{"type":"reasoning","text":"think"}`); d.Class != agent.DisplayReasoning || d.Text != "think" {
		t.Errorf("got %+v", d)
	}
}

func TestDisplayForDroid_ToolCall(t *testing.T) {
	d := disp(t, `{"type":"tool_call","toolName":"Execute","parameters":{"command":"ls -la"}}`)
	if d.Class != agent.DisplayToolCall || d.Tool != "Execute" || d.Text != "ls -la" {
		t.Errorf("got %+v", d)
	}
}

func TestDisplayForDroid_ToolResult_Counts(t *testing.T) {
	d := disp(t, `{"type":"tool_result","toolId":"LS","isError":false,"value":"a\nb\nc\n"}`)
	if d.Class != agent.DisplayToolResult || d.Tool != "LS" || d.IsError {
		t.Fatalf("got %+v", d)
	}
	if d.Lines != 3 || d.Bytes != 6 { // "a\nb\nc\n": 3 lines, 6 bytes
		t.Errorf("counts: Lines=%d Bytes=%d, want 3 / 6", d.Lines, d.Bytes)
	}
}

func TestDisplayForDroid_Error(t *testing.T) {
	d := disp(t, `{"type":"error","source":"cli","message":"Authentication failed"}`)
	if d.Class != agent.DisplayError || !d.IsError || d.Text != "Authentication failed" {
		t.Errorf("got %+v", d)
	}
}

func TestDisplayForDroid_Completion(t *testing.T) {
	if d := disp(t, `{"type":"completion","finalText":"all done"}`); d.Class != agent.DisplayFinal || d.Text != "all done" {
		t.Errorf("got %+v", d)
	}
}

func TestDisplayForDroid_Status(t *testing.T) {
	if d := disp(t, `{"type":"status","text":"running"}`); d.Class != agent.DisplayNotice || d.Text != "running" {
		t.Errorf("status text: got %+v", d)
	}
	// falls back to details when text is empty
	if d := disp(t, `{"type":"status","details":"phase 2"}`); d.Class != agent.DisplayNotice || d.Text != "phase 2" {
		t.Errorf("status details fallback: got %+v", d)
	}
}
