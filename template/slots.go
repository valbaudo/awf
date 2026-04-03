package template

import (
	"strings"
)

// Slot markers. The two-byte literals are centralized here so the slot-position arithmetic
// below uses len(slotOpen) instead of bare `2`s sprinkled across the function.
const (
	slotOpen  = "{{"
	slotClose = "}}"
)

// Slot is a `{{ … }}` region inside a host Template string. Start/End are byte offsets in the
// host source ([Start, End) covers the literal `{{` and `}}` markers as well as the inner text).
// Inner is the text between `{{` and `}}` (no trimming — the caller decides what counts as
// surrounding whitespace).
//
// Positions reported by errors from ParseRef(slot.Inner) are slot-local. To translate a slot-
// local offset N to the host string, callers compute hostPos = slot.Start + 2 + N (the +2 is
// the byte length of the `{{` opener). Slice 1.4's validator MUST perform this translation
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
		j := strings.Index(src[i:], slotOpen)
		if j < 0 {
			break
		}
		start := i + j
		innerStart := start + len(slotOpen)
		k := strings.Index(src[innerStart:], slotClose)
		// When `strings.Index` finds no closing `}}`, we report "unterminated" rather than
		// trying to disambiguate inputs like `{{{{` (which also nests). This is consistent:
		// nested-`{{` detection runs only AFTER a closing `}}` is found. Authors who write
		// `{{{{` get "unterminated"; authors who write `{{ {{ a }} }}` get "no nesting".
		if k < 0 {
			return nil, &SyntaxError{Pos: start, Msg: "unterminated `" + slotOpen + "`"}
		}
		inner := src[innerStart : innerStart+k]
		if strings.Contains(inner, slotOpen) {
			return nil, &SyntaxError{Pos: start, Msg: "`" + slotOpen + "` inside an open slot (no nesting)"}
		}
		end := innerStart + k + len(slotClose)
		out = append(out, Slot{Start: start, End: end, Inner: inner})
		i = end
	}
	return out, nil
}
