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

// parseInputFilesCSV parses the repeatable --input-files flag into name → path.
// Each flag occurrence is one element of entries (pflag StringArray does NOT split
// on commas), so paths containing commas are safe when supplied via the repeated
// form (`--input-files a=x --input-files b=y`). For back-compat with the legacy
// single CSV form (`--input-files a=x,b=y`), a lone element is split on commas —
// which is why a comma-containing path must be supplied via the repeated form (two
// or more occurrences) so the legacy split never applies. Each resolved entry must
// be `name=path` with non-empty halves; a duplicate name is rejected (last-wins
// would silently drop a supplied file). No entries → nil map (the caller's
// declared-vs-supplied check decides whether that is an error).
func parseInputFilesCSV(entries []string) (map[string]string, error) {
	// Drop empty occurrences (e.g. `--input-files ""`) before deciding the form.
	nonEmpty := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e) != "" {
			nonEmpty = append(nonEmpty, e)
		}
	}
	if len(nonEmpty) == 0 {
		return nil, nil
	}
	// A single occurrence may be the legacy comma-separated form, so split it; two
	// or more occurrences are taken literally so comma-bearing paths survive.
	pairs := nonEmpty
	if len(nonEmpty) == 1 {
		pairs = parseCSV(nonEmpty[0])
	}
	out := make(map[string]string, len(pairs))
	for _, e := range pairs {
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
	if len(out) == 0 {
		// A lone value that was all separators/whitespace (e.g. ",") survives the
		// non-empty filter but splits to zero pairs; return nil (not an empty map)
		// to honor the documented "no entries → nil map" contract.
		return nil, nil
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
	return requireRunDirForCommand(stateDir, runID, stderr, "", defaultStateIdentity)
}

func requireRunDirForCommand(stateDir, runID string, stderr io.Writer, command string, lookup stateIdentityLookup) int {
	runDir := filepath.Join(stateDir, "runs", runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "no run with id %q at %q. Did you mean a different --state-dir?\n", runID, runDir)
		} else {
			if command == "" {
				command = "awf"
			}
			return reportStateFailure(stderr, command, "stat run directory", stateDir, runDir, err, lookup, stateFailureInfra)
		}
		return ExitUsage
	}
	return ExitOK
}
