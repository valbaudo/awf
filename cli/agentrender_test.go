package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
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
func TestRender_ReasoningSkipsBlankLines(t *testing.T) {
	// leading blank: the first slot must carry a real thought, not an empty line.
	if got := render(ag(agent.DisplayReasoning, "", "\nreal thought\nmore", 0, 0, false)); got != "· thinking: real thought\n  more\n" {
		t.Errorf("leading blank: %q", got)
	}
	// interior blank: the 3-line budget spends on real content, not blanks.
	if got := render(ag(agent.DisplayReasoning, "", "head\n\ntail\nfourth", 0, 0, false)); got != "· thinking: head\n  tail\n  fourth\n" {
		t.Errorf("interior blank: %q", got)
	}
	// whitespace-only reasoning renders nothing (not a bare header).
	if got := render(ag(agent.DisplayReasoning, "", "\n\n\n", 0, 0, false)); got != "" {
		t.Errorf("whitespace-only must suppress: %q", got)
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

// formatEvent takes color as a plain bool, so the color==true path is tested
// directly without a fake TTY. Pins the exact ANSI wrap + reset on each colored
// branch (a swapped or unreset escape would otherwise regress silently — render()
// only ever exercises the plain path because a *bytes.Buffer is not a TTY).
func TestFormatEvent_ColorWrapsANSI(t *testing.T) {
	// error → red via colorize
	if got := formatEvent(ag(agent.DisplayError, "", "x", 0, 0, false), true); got != ansiRed+"✗ x"+ansiReset+"\n" {
		t.Errorf("error red: %q", got)
	}
	// init → dim via dim()
	if got := formatEvent(ag(agent.DisplayInit, "", "m · 2 tools", 0, 0, false), true); got != ansiDim+"· m · 2 tools"+ansiReset+"\n" {
		t.Errorf("init dim: %q", got)
	}
	// tool result → green header (colorize) + dimmed body line (dim)
	if got := formatEvent(ag(agent.DisplayToolResult, "T", "body", 1, 4, false), true); got != ansiGreen+"✓ T — 1 lines, 4 B"+ansiReset+"\n"+ansiDim+"    body"+ansiReset+"\n" {
		t.Errorf("tool result green+dim: %q", got)
	}
}

// End-to-end: a fake-agent workflow run must render per-kind lines through the
// engine drain → injected renderer → tap. Proves the whole chain, not just units.
func TestCLIRun_RendersAgentEventsEndToEnd(t *testing.T) {
	backend := container.NewFake()
	f := fake.New("test/fake").WithVersion("v1").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{
			{Kind: "tool_call", Stream: "stdout", Display: agent.EventDisplay{Class: agent.DisplayToolCall, Tool: "Execute", Text: "ls -la"}},
			{Kind: "completion", Stream: "stdout", Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: "all done"}},
		},
	})
	var reg agent.Registry
	if err := reg.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var tap, stdout, stderr bytes.Buffer
	r := &Runner{
		Backend:       backend,
		Resolver:      &reg,
		AgentEventTap: &tap,
		IDGen:         &clock.Fake{IDs: []string{"render-run-1"}},
	}
	rc := r.Run([]string{"run", "--state-dir", t.TempDir(), "testdata/render-probe.yaml"}, &stdout, &stderr)
	if rc != ExitOK {
		t.Fatalf("run rc=%d, stderr=%s", rc, stderr.String())
	}
	out := tap.String()
	if !strings.Contains(out, "→ Execute(ls -la)\n") || !strings.Contains(out, "all done\n") {
		t.Errorf("tap did not render per-kind agent events end-to-end:\n%s", out)
	}
}
