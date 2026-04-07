// Package cli assembles the command-line surface. Slice 1.6 ships `awf validate <path>`;
// later phases add `run`/`resume` (Phase 2), `signal`/`cancel`/`pause` (Phase 3), and
// `inspect`/`trace`/`ls` (Phase 6). The entry point is Run, not init() or a package-level
// CLI framework, so tests drive the full surface with bytes.Buffer for IO and an int return
// for the exit code — no real os.Exit ever called from package code.
package cli

import (
	"fmt"
	"io"
)

// Exit codes are part of the CLI contract — downstream tooling (CI, IDE extensions, scripts)
// will branch on them. Locked by TestValidateExitCodesAreStable.
const (
	ExitOK      = 0 // success: validation produced zero error-severity diagnostics (warnings are OK)
	ExitInvalid = 1 // validation produced ≥1 error-severity diagnostic
	ExitUsage   = 2 // bad arguments, unreadable file, or loader-stage failure (parse error, path escape, …)
)

// Run dispatches a single CLI invocation. args is os.Args[1:] (subcommand first, then its
// arguments). stdout and stderr are passed in so tests can capture output. The returned int
// is the exit code; cmd/awf wraps with os.Exit.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "validate":
		return cliValidate(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return ExitOK
	default:
		fprintf(stderr, "awf: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return ExitUsage
	}
}

func printUsage(w io.Writer) {
	fprintln(w, "usage: awf <subcommand> [arguments]")
	fprintln(w, "")
	fprintln(w, "subcommands:")
	fprintln(w, "  validate <path>   parse, validate, and print the workflow's digest")
	fprintln(w, "  help              print this usage")
}

// fprintf / fprintln are fmt.Fprintf / fmt.Fprintln with the (n int, err error) return
// explicitly discarded. The CLI writes to bytes.Buffer (tests) or os.Stdout/os.Stderr
// (production); writes to those targets cannot fail in any realistic operational scenario,
// and surfacing an "error printing the error message" would be worse than silently dropping
// it. Centralizing the discard here documents WHY in one place instead of repeating
// `_, _ = fmt.Fprintf(...)` at every call site (errcheck-clean via the explicit discard).
func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func fprintln(w io.Writer, s string) {
	_, _ = fmt.Fprintln(w, s)
}
