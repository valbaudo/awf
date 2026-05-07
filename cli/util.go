package cli

import (
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// parseCSV splits a comma-separated string into a clean []string. Trims
// whitespace, drops empties. Empty input → nil slice (NOT empty slice —
// the buildAgentRegistry path treats nil as "no allowlist").
func parseCSV(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' })
	out := parts[:0]
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseRunIDFirst parses a "<run-id> [flags]" command line where the run id is
// the leading positional (stdlib flag stops at the first non-flag, so flags must
// follow the id). Returns the run id and ok=true to proceed; on any usage problem
// it prints usage (to stdout for -h/--help → ExitOK, else stderr → ExitUsage) and
// returns ok=false with the exit code the caller should return. cmd is the
// "awf <subcommand>" prefix for error messages.
func parseRunIDFirst(fs *flag.FlagSet, args []string, cmd string, usage func(io.Writer), stdout, stderr io.Writer) (runID string, exit int, ok bool) {
	if len(args) == 0 {
		usage(stderr)
		return "", ExitUsage, false
	}
	if strings.HasPrefix(args[0], "-") {
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return "", ExitOK, false
		}
		usage(stderr)
		return "", ExitUsage, false
	}
	runID = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return "", ExitOK, false
		}
		fprintf(stderr, "%s: %v\n", cmd, err)
		usage(stderr)
		return "", ExitUsage, false
	}
	if fs.NArg() != 0 {
		usage(stderr)
		return "", ExitUsage, false
	}
	return runID, ExitOK, true
}

// requireRunDir checks that <stateDir>/runs/<runID> exists. Returns ExitOK if
// present; ExitUsage with a helpful stderr message otherwise. Shared by the
// signal, pause, and cancel subcommands (rule-of-three: 3 identical call sites).
func requireRunDir(stateDir, runID string, stderr io.Writer) int {
	runDir := filepath.Join(stateDir, "runs", runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "no run with id %q at %q\n", runID, runDir)
		} else {
			fprintf(stderr, "stat run dir %q: %v\n", runDir, err)
		}
		return ExitUsage
	}
	return ExitOK
}
