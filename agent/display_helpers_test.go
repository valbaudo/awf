package agent

import (
	"strings"
	"testing"
)

func TestCountLines(t *testing.T) {
	cases := map[string]int{"": 0, "x": 1, "a\nb": 2, "a\nb\n": 2, "a\nb\nc\n": 3, "a\nb\nc": 3}
	for in, want := range cases {
		if got := CountLines(in); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestElide_HeadAndTail(t *testing.T) {
	body := "L1\nL2\nL3\nL4\nL5\nL6\nL7\nL8\nL9\nL10"
	got := Elide(body, 3, 3)
	if !strings.Contains(got, "L1") || !strings.Contains(got, "L10") {
		t.Errorf("Elide must keep head AND tail: %q", got)
	}
	if strings.Contains(got, "L5") {
		t.Errorf("Elide must drop the middle: %q", got)
	}
	if !strings.Contains(got, "4 more lines") {
		t.Errorf("Elide must mark the hidden count: %q", got)
	}
	if Elide("a\nb", 3, 3) != "a\nb" {
		t.Errorf("short input must pass through")
	}
}

func TestElide_SingularMarker(t *testing.T) {
	// exactly headN+tailN+1 lines → 1 hidden → singular "line"
	got := Elide("a\nb\nc", 1, 1)
	if !strings.Contains(got, "1 more line ") {
		t.Errorf("singular marker expected: %q", got)
	}
}

func TestSummarizeToolInput(t *testing.T) {
	cases := map[string]string{
		`{"command":"go test","verbose":true}`: "go test",
		`{"directory_path":"/repo"}`:           "/repo",
		`{"file_path":"main.go","limit":50}`:   "main.go",
		`{"pattern":"func.*"}`:                 "func.*",
		`{"misc":42}`:                          "42",
	}
	for in, want := range cases {
		if got := SummarizeToolInput([]byte(in)); got != want {
			t.Errorf("SummarizeToolInput(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestClip_BoundsLongLine(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := clip(long)
	if len(got) > maxDisplayLine+32 {
		t.Errorf("clip did not bound: len=%d", len(got))
	}
	if !strings.Contains(got, "bytes") {
		t.Errorf("clip should mark the cut: %q", got[len(got)-40:])
	}
	if clip("short") != "short" {
		t.Errorf("clip must pass short strings through unchanged")
	}
}

func TestElide_ClipsPathologicalLine(t *testing.T) {
	got := Elide(strings.Repeat("y", 1_000_000), 4, 4)
	if len(got) > maxDisplayLine+32 {
		t.Errorf("Elide must clip a giant single line: len=%d", len(got))
	}
}

func TestSummarizeToolInput_ClipsHugeValue(t *testing.T) {
	huge := `{"command":"` + strings.Repeat("z", 1_000_000) + `"}`
	if got := SummarizeToolInput([]byte(huge)); len(got) > maxDisplayLine+32 {
		t.Errorf("SummarizeToolInput must clip a huge value: len=%d", len(got))
	}
}
