package cli

import "os"

// defaultStateDir is the value seeded as the default for every --state-dir flag:
// the AWF_STATE_DIR environment variable when set and non-empty, otherwise ".awf".
//
// Seeding the flag DEFAULT (rather than post-processing the flag value) gives the
// precedence explicit flag > AWF_STATE_DIR > .awf for free: pflag uses the default
// only when --state-dir is absent, so an explicit flag always wins and no per-
// call-site resolution logic is needed.
func defaultStateDir() string {
	if v := os.Getenv("AWF_STATE_DIR"); v != "" {
		return v
	}
	return ".awf"
}
