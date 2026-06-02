package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/valbaudo/awf/agent"
)

const (
	glyphMeta    = "·"
	glyphCall    = "→"
	glyphOK      = "✓"
	glyphFail    = "✗"
	reasonPrefix = "· thinking: "
)
const (
	reasoningMaxLines = 3   // reasoning lines shown before eliding
	textLineBudget    = 200 // per-line rune cap for one-liners
	fallbackPreview   = 120 // Other-class raw-payload preview bytes
)
const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
)

// newAgentEventRenderer returns a per-event formatter for the live tap. Color is
// decided ONCE from w (a terminal not suppressed by NO_COLOR); the returned func
// writes to the io.Writer the engine hands it (the same tap). The engine stays
// presentation-agnostic and just calls this per AgentEvent.
func newAgentEventRenderer(w io.Writer) func(io.Writer, agent.AgentEvent) {
	color := wantColor(w)
	return func(out io.Writer, ev agent.AgentEvent) {
		_, _ = io.WriteString(out, formatEvent(ev, color))
	}
}

// newDispatcherEventRenderer builds the renderer the CLI hands the engine. tapW
// is the same writer used as AgentEventTap, so the color decision matches where
// the lines go.
func (r *Runner) newDispatcherEventRenderer(tapW io.Writer) func(io.Writer, agent.AgentEvent) {
	return newAgentEventRenderer(tapW)
}

// wantColor: ANSI only when w is a character device and NO_COLOR is unset.
func wantColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func dim(s string, color bool) string {
	if !color {
		return s
	}
	return ansiDim + s + ansiReset
}
func colorize(s, code string, color bool) string {
	if !color {
		return s
	}
	return code + s + ansiReset
}

// formatEvent renders one event per its normalized Display class; empty-text
// events are suppressed; unknown/Other falls back to the terse "[kind] preview".
func formatEvent(ev agent.AgentEvent, color bool) string {
	d := ev.Display
	switch d.Class {
	case agent.DisplayAssistant, agent.DisplayFinal:
		if d.Text == "" {
			return ""
		}
		return d.Text + "\n" // the answer: full text, full brightness
	case agent.DisplayReasoning:
		return formatReasoning(d.Text, color)
	case agent.DisplayInit, agent.DisplayNotice:
		if d.Text == "" {
			return ""
		}
		return dim(glyphMeta+" "+oneLine(d.Text), color) + "\n"
	case agent.DisplayToolCall:
		if d.Tool == "" && d.Text == "" {
			return ""
		}
		return dim(glyphCall+" "+d.Tool+"("+oneLine(d.Text)+")", color) + "\n"
	case agent.DisplayToolResult:
		return formatToolResult(d, color)
	case agent.DisplayError:
		return colorize(glyphFail+" "+oneLine(d.Text), ansiRed, color) + "\n"
	default: // DisplayOther → terse fallback (bounded; no raw JSON flood)
		p := ev.Payload
		if len(p) > fallbackPreview {
			p = p[:fallbackPreview]
		}
		return fmt.Sprintf("[%s] %s\n", ev.Kind, p)
	}
}

// formatReasoning shows up to reasoningMaxLines of the (already model-bounded)
// reasoning text, dimmed: first line prefixed "· thinking: ", the rest indented.
func formatReasoning(text string, color bool) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	for i, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if i >= reasoningMaxLines {
			break
		}
		prefix := "  "
		if i == 0 {
			prefix = reasonPrefix
		}
		b.WriteString(dim(prefix+truncate(ln, textLineBudget), color) + "\n")
	}
	return b.String()
}

// formatToolResult: status glyph + tool + size header, then each line of the
// already-elided head+tail body, dimmed and indented. The full body is never
// printed (it's in the journal).
func formatToolResult(d agent.EventDisplay, color bool) string {
	glyph, code := glyphOK, ansiGreen
	if d.IsError {
		glyph, code = glyphFail, ansiRed
	}
	var b strings.Builder
	b.WriteString(colorize(fmt.Sprintf("%s %s — %d lines, %s", glyph, d.Tool, d.Lines, humanizeBytes(d.Bytes)), code, color) + "\n")
	for _, line := range strings.Split(d.Text, "\n") {
		if line == "" {
			continue
		}
		b.WriteString(dim("    "+truncate(line, textLineBudget), color) + "\n")
	}
	return b.String()
}

// oneLine collapses to the first line, rune-capped.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, textLineBudget)
}

// truncate caps s to n RUNES (not bytes — byte slicing would split a multibyte
// rune into mojibake), appending "…" when cut. (Display-column width for wide
// glyphs is a later refinement; rune-count prevents the broken-glyph bug.)
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

func humanizeBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
