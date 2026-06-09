package template

import "strings"

// ParseArtifactRef parses a static artifact reference "step.<id>.files.<name>"
// into (id, name). Trims surrounding whitespace (parity with the ref-validation
// path, validate_refs.go's TrimSpace on slot inners). ok=false for any other
// shape (wrong root, wrong arity, index segments, or {{ }} which the lexer
// rejects). Shared by the IR validator and the engine resolver so the grammar
// lives in one place.
func ParseArtifactRef(raw string) (id, name string, ok bool) {
	r, err := ParseRef(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false
	}
	g := r.Segments
	if len(g) != 4 ||
		g[0].IsIndex || g[0].Ident != "step" ||
		g[1].IsIndex ||
		g[2].IsIndex || g[2].Ident != "files" ||
		g[3].IsIndex {
		return "", "", false
	}
	return g[1].Ident, g[3].Ident, true
}

// ParseAssetRef parses a static asset reference "asset.<id>" into id. Trims
// surrounding whitespace (parity with ParseArtifactRef). ok=false for any other
// shape.
func ParseAssetRef(raw string) (id string, ok bool) {
	r, err := ParseRef(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	g := r.Segments
	if len(g) != 2 ||
		g[0].IsIndex || g[0].Ident != "asset" ||
		g[1].IsIndex {
		return "", false
	}
	return g[1].Ident, true
}
