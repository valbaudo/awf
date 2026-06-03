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
// A `{{` immediately preceded by a single `\` is an ESCAPED opener (host-level escape, AWF
// §7): it does NOT open a slot, and the `}}` that would close it is inert literal text.
// Authors write `\{{` to embed a literal `{{` (e.g. an SSTI payload or prose teaching
// templating). The backslash is consumed at substitution time (see Substitute); here the
// escaped region is simply skipped. `\` is special only in this position.
//
// Used by validation: for every ir.Template field (e.g. CodeStep.Run, Container.Image,
// idempotency_key), call Slots, then ParseRef each slot's Inner. Per AWF §7, substitution
// targets are references — never full expressions — so ParseRef (not ParseExpr) is the
// right consumer. Position translation: see the Slot doc comment.
func Slots(src string) ([]Slot, error) {
	slots, _, err := scan(src)
	return slots, err
}

// scan is the shared scanner behind Slots (validation) and Substitute (rendering). It returns
// the `{{ … }}` slots AND the byte offsets of the `\` that escapes each escaped `{{` opener.
// Substitute needs the escape offsets to consume the `\` and emit a literal `{{`; the validator
// only needs the slots (escaped openers are already absent from them). Keeping ONE scanner
// guarantees the two callers agree on exactly which `{{` are escaped.
func scan(src string) (slots []Slot, escapes []int, err error) {
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], slotOpen)
		if j < 0 {
			break
		}
		start := i + j
		// Escaped opener: a single `\` directly before this `{{`. Skip past the `{{` so the
		// region is not scanned as a slot (and its `}}` stays literal). Record the `\` offset
		// for Substitute. `\` is special only here — a `\` anywhere else is literal text.
		if start > 0 && src[start-1] == '\\' {
			escapes = append(escapes, start-1)
			i = start + len(slotOpen)
			continue
		}
		innerStart := start + len(slotOpen)
		k := strings.Index(src[innerStart:], slotClose)
		// When `strings.Index` finds no closing `}}`, we report "unterminated" rather than
		// trying to disambiguate inputs like `{{{{` (which also nests). This is consistent:
		// nested-`{{` detection runs only AFTER a closing `}}` is found. Authors who write
		// `{{{{` get "unterminated"; authors who write `{{ {{ a }} }}` get "no nesting".
		if k < 0 {
			return nil, nil, &SyntaxError{Pos: start, Msg: "unterminated `" + slotOpen + "`"}
		}
		inner := src[innerStart : innerStart+k]
		if strings.Contains(inner, slotOpen) {
			return nil, nil, &SyntaxError{Pos: start, Msg: "`" + slotOpen + "` inside an open slot (no nesting)"}
		}
		end := innerStart + k + len(slotClose)
		slots = append(slots, Slot{Start: start, End: end, Inner: inner})
		i = end
	}
	return slots, escapes, nil
}
