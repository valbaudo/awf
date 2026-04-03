package template

import (
	"errors"
	"strings"
	"testing"
)

func TestSlotsHappyPath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Slot
	}{
		{"single slot", "{{ a }}", []Slot{{Start: 0, End: 7, Inner: " a "}}},
		{"slot in larger text", "foo {{ a }} bar", []Slot{{Start: 4, End: 11, Inner: " a "}}},
		{"adjacent slots", "{{ a }}{{ b }}", []Slot{
			{Start: 0, End: 7, Inner: " a "},
			{Start: 7, End: 14, Inner: " b "},
		}},
		{"suffix concatenation", "{{ run.id }}:pr", []Slot{{Start: 0, End: 12, Inner: " run.id "}}},
		{"no slots", "no slots here", nil},
		{"empty input", "", nil},
		{"empty inner", "{{}}", []Slot{{Start: 0, End: 4, Inner: ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Slots(c.src)
			if err != nil {
				t.Fatalf("Slots(%q): %v", c.src, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("Slots(%q) = %+v, want %+v", c.src, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("slot[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestSlotsErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantPos int
		msgSub  string
	}{
		{"{{", 0, "unterminated"},
		{"{{ a", 0, "unterminated"},
		{"foo {{ a", 4, "unterminated"},
		{"{{ {{ a }} }}", 0, "no nesting"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := Slots(c.src)
			if err == nil {
				t.Fatalf("Slots(%q) expected error containing %q", c.src, c.msgSub)
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err is %T, want *SyntaxError: %v", err, err)
			}
			if se.Pos != c.wantPos {
				t.Errorf("err.Pos = %d, want %d (msg: %q)", se.Pos, c.wantPos, se.Msg)
			}
			if !strings.Contains(se.Msg, c.msgSub) {
				t.Errorf("err.Msg = %q, want substring %q", se.Msg, c.msgSub)
			}
		})
	}
}

// Smoke test: the slot scanner cooperates with ParseRef for the validator's typical pattern.
func TestSlotsFeedingParseRef(t *testing.T) {
	src := `./scan.sh "{{ input.cve_id }}" --out "{{ run.id }}.log"`
	slots, err := Slots(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	for _, sl := range slots {
		inner := strings.TrimSpace(sl.Inner)
		if _, err := ParseRef(inner); err != nil {
			t.Errorf("ParseRef(%q): %v", inner, err)
		}
	}
}

// A `}}` outside any open slot is just literal text — the scanner doesn't fabricate slots.
func TestSlotsStrayCloseIsLiteral(t *testing.T) {
	got, err := Slots("hello }} world")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected no slots in %q, got %+v", "hello }} world", got)
	}
}
