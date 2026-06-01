package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ToolResultHeadTail is the default head/tail line budget for a tool result's
// preview, passed to Elide.
const ToolResultHeadTail = 4

// maxDisplayLine bounds a single line (or tool-arg summary) placed in
// EventDisplay.Text. Display is json:"-" — it never journals — but every
// AgentEvent stays buffered in memory until the step commits, so a pathological
// tool output (a minified blob, a binary dump, a megabyte-long command) would
// otherwise pin megabytes per event for no durable benefit. The full bytes still
// live in the raw Payload; this only bounds the live preview. A renderer
// truncates again for the terminal — maxDisplayLine is deliberately wider than a
// terminal so the renderer keeps room while memory stays bounded.
const maxDisplayLine = 512

// CountLines returns the human line count of s: 0 for empty; otherwise the number
// of "\n"-separated lines, where a trailing newline does NOT add a phantom line.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// Elide returns a bounded head+tail view of s: if s has <= headN+tailN lines, the
// whole thing (trailing newline trimmed); else the first headN lines, a
// "… N more line(s) …" marker, and the last tailN lines. Every returned line is
// capped to maxDisplayLine bytes (clip), so neither a huge line count nor a single
// pathological line can bloat EventDisplay.Text. The tail is kept because errors
// and results land at the end.
func Elide(s string, headN, tailN int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= headN+tailN {
		return strings.Join(clipLines(lines), "\n")
	}
	hidden := len(lines) - headN - tailN
	noun := "lines"
	if hidden == 1 {
		noun = "line"
	}
	out := append([]string{}, lines[:headN]...)
	out = append(out, fmt.Sprintf("… %d more %s …", hidden, noun))
	out = append(out, lines[len(lines)-tailN:]...)
	return strings.Join(clipLines(out), "\n")
}

// clipLines caps each line in place (the slices passed in are freshly built).
func clipLines(lines []string) []string {
	for i, l := range lines {
		lines[i] = clip(l)
	}
	return lines
}

// clip caps s to maxDisplayLine bytes, backing up to a UTF-8 rune boundary so it
// never splits a multi-byte char, and appends a byte-count marker when it cuts.
func clip(s string) string {
	if len(s) <= maxDisplayLine {
		return s
	}
	end := maxDisplayLine
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + fmt.Sprintf("…(+%d bytes)", len(s)-end)
}

// salientToolArgKeys are the tool-argument keys worth showing inline, in order.
var salientToolArgKeys = []string{"command", "file_path", "path", "directory_path", "pattern", "query", "url"}

// SummarizeToolInput picks the most meaningful value from a tool call's argument
// object for a one-line display, without a per-tool registry: a salient key first,
// else the first scalar by sorted key. The result is clipped to maxDisplayLine.
// Returns "" if raw is empty or not a JSON object.
func SummarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range salientToolArgKeys {
		if v, ok := m[k]; ok {
			return clip(scalarString(v))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if s := scalarString(m[k]); s != "" {
			return clip(s)
		}
	}
	return ""
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}
