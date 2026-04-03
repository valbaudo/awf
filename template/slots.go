package template

import (
	"strings"
)

// Slot is a `{{ … }}` region inside a host Template string. Start/End are byte offsets in the
// host source ([Start, End) covers the literal `{{` and `}}` markers as well as the inner text).
// Inner is the text between `{{` and `}}` (no trimming — the caller decides what counts as
// surrounding whitespace).
//
// Positions reported by errors from ParseRef(slot.Inner) are slot-local. To translate a slot-
// local offset N to the host string, callers compute hostPos = slot.Start + 2 + N (the +2
// accounts for the `{{` marker length). Slice 1.4's validator MUST perform this translation
// before emitting a Diagnostic, otherwise reported positions are off by `slot.Start + 2`.
type Slot struct {
	Start, End int
	Inner      string
}

// Slots scans src and returns every `{{ … }}` region in left-to-right order. Returns a
// *SyntaxError on an unterminated `{{` or on a `{{` that appears before a matching `}}`
// (no nesting). A stray `}}` outside any open slot is treated as literal text.
//
// Used by validation: for every ir.Template field (e.g. CodeStep.Run, Container.Image,
// idempotency_key), call Slots, then ParseRef each slot's Inner. Per AWF §7, substitution
// targets are references — never full expressions — so ParseRef (not ParseExpr) is the
// right consumer. Position translation: see the Slot doc comment.
func Slots(src string) ([]Slot, error) {
	var out []Slot
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], "{{")
		if j < 0 {
			break
		}
		start := i + j
		k := strings.Index(src[start+2:], "}}")
		if k < 0 {
			return nil, &SyntaxError{Pos: start, Msg: "unterminated `{{`"}
		}
		inner := src[start+2 : start+2+k]
		if strings.Contains(inner, "{{") {
			return nil, &SyntaxError{Pos: start, Msg: "`{{` inside an open slot (no nesting)"}
		}
		end := start + 2 + k + 2
		out = append(out, Slot{Start: start, End: end, Inner: inner})
		i = end
	}
	return out, nil
}
