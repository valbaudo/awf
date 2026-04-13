package template

import "strings"

// UnwrapEnvelope strips a single `{{ ... }}` envelope (with optional surrounding
// whitespace) from src, returning the inner expression. If src does not start
// with `{{` and end with `}}`, it is returned unchanged.
//
// Used by callers that parse a single-expression field (if.cond / loop.until /
// gate.until — spec §5.1 / §5.2 / §5.5) where the YAML surface wraps the
// expression in `{{ }}` for human readability but the parser wants the bare
// expression. This is intentionally narrow: it does NOT scan for multiple slots
// (use Slots for that — the field's surface contract is "exactly one expression").
//
// Mirrors ir/validate_refs.go's previously-private unwrapEnvelope; promoting it
// here gives the runtime (engine.runIf, engine.runLoop) the same logic without
// crossing the ir → template layer.
func UnwrapEnvelope(src string) string {
	s := strings.TrimSpace(src)
	if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
		return strings.TrimSpace(s[2 : len(s)-2])
	}
	return src
}
