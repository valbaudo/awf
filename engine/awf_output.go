package engine

import "strings"

// awfOutputTempPath returns the per-step path the Backend's Exec reads
// $AWF_OUTPUT from, per AWF spec §4.1. Format:
//
//	/tmp/awf/<sanitized-node-path>.json
//
// where sanitize replaces any character outside [a-zA-Z0-9_-] with "_". The
// sanitization is one-way (we never reverse-parse this string) and preserves
// hyphens since the spec §8 addressing form uses them (e.g. `iter-3`).
//
// Deterministic per node path — each retry attempt of the same step writes to
// the same file, the previous attempt's content is overwritten. The capture
// step reads the freshest bytes (CLAUDE.md determinism invariant: no rand /
// uuid / time.Now in engine logic; the path is a pure function of inputs).
//
// Slice 4.2 (Phase 4) introduces this helper. The Phase 2 fake never sees
// the AWF_OUTPUT path (it ignores Env; its scripted AWFOutput field is the
// dispatcher's source of truth on the fake path — see local_dispatcher.go
// runCode). The Docker Backend's Exec passes Env through to the container
// unchanged; the author's script writes to "$AWF_OUTPUT".
func awfOutputTempPath(nodePath string) string {
	return "/tmp/awf/" + sanitizeForFilename(nodePath) + ".json"
}

// sanitizeForFilename returns s with every rune outside [a-zA-Z0-9_-] replaced
// by '_'. Uses strings.Map (stdlib idiomatic; one allocation in the worst
// case).
func sanitizeForFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		}
		return '_'
	}, s)
}
