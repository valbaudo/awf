package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
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

// parseInputFilesCSV parses the --input-files flag value: a comma-separated
// list of `name=path` entries. Returns name → path. An empty input yields a nil
// map (no files supplied — the caller's declared-vs-supplied check decides if
// that is an error). Each entry must contain exactly one `=`; the name and path
// must both be non-empty. A duplicate name is rejected (last-wins would silently
// drop a supplied file). Mirrors parseCSV's comma-splitting + whitespace-trim.
func parseInputFilesCSV(s string) (map[string]string, error) {
	entries := parseCSV(s)
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		name, path, ok := strings.Cut(e, "=")
		name = strings.TrimSpace(name)
		path = strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("malformed --input-files entry %q: want name=path", e)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--input-files supplies %q more than once", name)
		}
		out[name] = path
	}
	return out, nil
}

// parseSinglePositional parses a command line that takes exactly one positional
// (a run id, or a workflow path) plus flags in any position. pflag parses flags
// interspersed with positionals (GNU style), so the positional may appear before
// OR after any flag — `awf inspect r1 --tokens` and `awf inspect --tokens r1`
// both work. Returns the positional and ok=true to proceed; on any usage problem
// it prints usage (to stdout for -h/--help → ExitOK, else stderr → ExitUsage) and
// returns ok=false with the exit code the caller should return. cmd is the
// "awf <subcommand>" prefix for error messages.
func parseSinglePositional(fs *pflag.FlagSet, args []string, cmd string, usage func(io.Writer), stdout, stderr io.Writer) (positional string, exit int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			usage(stdout)
			return "", ExitOK, false
		}
		fprintf(stderr, "%s: %v\n", cmd, err)
		usage(stderr)
		return "", ExitUsage, false
	}
	if fs.NArg() != 1 {
		usage(stderr)
		return "", ExitUsage, false
	}
	return fs.Arg(0), ExitOK, true
}

// requireRunDir checks that <stateDir>/runs/<runID> exists. Returns ExitOK if
// present; ExitUsage with a helpful stderr message otherwise. Shared by the
// signal, pause, and cancel subcommands (rule-of-three: 3 identical call sites).
func requireRunDir(stateDir, runID string, stderr io.Writer) int {
	runDir := filepath.Join(stateDir, "runs", runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "no run with id %q at %q. Did you mean a different --state-dir?\n", runID, runDir)
		} else {
			fprintf(stderr, "stat run dir %q: %v\n", runDir, err)
		}
		return ExitUsage
	}
	return ExitOK
}
