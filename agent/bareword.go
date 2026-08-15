package agent

// IsBareWord reports whether s is safe to interpolate as a bare TOML value
// (agent CLIs take `-c key=<value>` overrides) inside a shell-quoted command
// line: non-empty, lowercase ASCII letters only, bounded length.
//
// Adapters validate effort-tier values with this predicate INSTEAD of an
// enum: awf guarantees TRANSPORT safety; the agent CLI owns its vocabulary.
// The enum this replaces (verified against codex v0.131.0) rejected valid
// tiers codex added later (max, ultra in v0.146.0) — a value mirror couples
// awf to the CLI's release cadence (2026-08-15). A bogus tier now fails at
// the CLI/API at run time, loudly, instead of a stale enum failing a valid
// one at validate time.
func IsBareWord(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}
