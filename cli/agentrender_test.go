package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
)

func render(ev agent.AgentEvent) string {
	var b bytes.Buffer
	newAgentEventRenderer(&b)(&b, ev) // &bytes.Buffer is not a TTY → plain
	return b.String()
}

func TestRender_AssistantFull(t *testing.T) {
	if got := render(ag(agent.DisplayAssistant, "", "the full answer\nlines", 0, 0, false)); got != "the full answer\nlines\n" {
		t.Errorf("assistant verbatim: %q", got)
	}
}
func TestRender_AssistantEmptySuppressed(t *testing.T) {
	if got := render(ag(agent.DisplayAssistant, "", "", 0, 0, false)); got != "" {
		t.Errorf("empty assistant text must be suppressed: %q", got)
	}
}
func TestRender_ToolCall(t *testing.T) {
	if got := render(ag(agent.DisplayToolCall, "Execute", "ls -la", 0, 0, false)); got != "→ Execute(ls -la)\n" {
		t.Errorf("got %q", got)
	}
}
func TestRender_ToolResult_HeaderAndElidedBodyWithTail(t *testing.T) {
	// Text is the already-elided head+tail (as the adapter produces).
	got := render(ag(agent.DisplayToolResult, "Execute", "L1\nL2\n… 140 more lines …\nL145\nL146", 146, 4096, false))
	if !strings.HasPrefix(got, "✓ Execute — 146 lines, 4.0 KB\n") {
		t.Errorf("header: %q", got)
	}
	if !strings.Contains(got, "    L1\n") || !strings.Contains(got, "    L146\n") || !strings.Contains(got, "    … 140 more lines …\n") {
		t.Errorf("body must show head, tail, and the elision marker (dim/indented): %q", got)
	}
}
func TestRender_ToolResultError(t *testing.T) {
	if got := render(ag(agent.DisplayToolResult, "Execute", "boom", 1, 4, true)); !strings.HasPrefix(got, "✗ Execute") {
		t.Errorf("error result uses ✗: %q", got)
	}
}
func TestRender_ReasoningUpTo3Lines(t *testing.T) {
	got := render(ag(agent.DisplayReasoning, "", "one\ntwo\nthree\nfour", 0, 0, false))
	if got != "· thinking: one\n  two\n  three\n" {
		t.Errorf("reasoning should show up to 3 lines: %q", got)
	}
}
func TestRender_Error(t *testing.T) {
	if got := render(ag(agent.DisplayError, "", "auth failed", 0, 0, false)); got != "✗ auth failed\n" {
		t.Errorf("got %q", got)
	}
}
func TestRender_InitAndNotice(t *testing.T) {
	if got := render(ag(agent.DisplayInit, "", "claude-opus-4-8 · 24 tools", 0, 0, false)); got != "· claude-opus-4-8 · 24 tools\n" {
		t.Errorf("init: %q", got)
	}
	if got := render(ag(agent.DisplayNotice, "", "retrying", 0, 0, false)); got != "· retrying\n" {
		t.Errorf("notice: %q", got)
	}
}
func TestRender_OtherFallback(t *testing.T) {
	if got := render(agent.AgentEvent{Kind: "weird", Payload: []byte("raw bytes")}); got != "[weird] raw bytes\n" {
		t.Errorf("fallback: %q", got)
	}
}
func TestRender_NoANSIWhenNotTTY(t *testing.T) {
	if strings.Contains(render(ag(agent.DisplayError, "", "x", 0, 0, false)), "\x1b[") {
		t.Error("plain mode must emit no ANSI")
	}
}
func TestTruncate_RuneAware(t *testing.T) {
	// 4 multibyte runes; truncating to 2 must not split a rune.
	if got := truncate("héllo", 2); got != "hé…" {
		t.Errorf("truncate rune-aware: got %q", got)
	}
}
func TestHumanizeBytes(t *testing.T) {
	for in, want := range map[int]string{0: "0 B", 512: "512 B", 1536: "1.5 KB", 1572864: "1.5 MB"} {
		if got := humanizeBytes(in); got != want {
			t.Errorf("humanizeBytes(%d)=%q want %q", in, got, want)
		}
	}
}

func ag(c agent.DisplayClass, tool, text string, lines, bytesN int, isErr bool) agent.AgentEvent {
	return agent.AgentEvent{Display: agent.EventDisplay{Class: c, Tool: tool, Text: text, Lines: lines, Bytes: bytesN, IsError: isErr}}
}
