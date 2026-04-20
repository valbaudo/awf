// Package cli assembles the command-line surface. Slice 1.6 shipped
// `awf validate <path>`; slice 2.5 added `awf run <file> [--input <json>]
// [--run-id <id>] [--state-dir <dir>]`; slice 2.6 added `awf resume
// <run-id> <path> [--state-dir <dir>]`. Later phases add `signal`/
// `cancel`/`pause` (Phase 3) and `inspect`/`trace`/`ls` (Phase 6).
// The entry point is Runner.Run, not init() or a package-level
// CLI framework, so tests drive the full surface with bytes.Buffer for IO
// and an int return for the exit code — no real os.Exit ever called from
// package code.
//
// Runner holds the production-vs-test seams: Backend (Phase 2 wires
// container.Fake by default — Phase 4 swaps in Docker, slice 2.5 Design
// question 1) and IDGen. The Clock is inlined as clock.System{} at the
// engine.Run call site — slice 2.5 has no test that needs CLI-level fake-
// clock injection; the engine's own retry tests already cover deterministic-
// time paths via the engine's clock parameter. If a future slice needs CLI-
// level clock injection, add the Runner.Clock field then (3-line patch).
package cli

import (
	"fmt"
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
)

// Exit codes are part of the CLI contract — downstream tooling (CI, IDE
// extensions, scripts) will branch on them. Locked by tests.
//
// Note ExitInvalid and ExitRunFailed share the numeric value 1 — both mean
// "the input was structurally fine but the operation didn't succeed." We keep
// them as separate Go constants so each subcommand can name its own failure
// mode for readability.
const (
	ExitOK        = 0 // success: validation produced zero error-severity diagnostics, OR run terminated ok
	ExitInvalid   = 1 // `awf validate` produced ≥1 error-severity diagnostic (also returned by `awf run` if its pre-run validation finds errors)
	ExitUsage     = 2 // bad arguments, unreadable file, or loader-stage failure (parse error, path escape, missing compose)
	ExitRunFailed = 1 // `awf run` completed but the run terminated as retryable_failure / permanent_failure
)

// Runner holds the dependencies that vary between production and tests. The
// zero value is unusable — callers (production: cli.Run; tests: direct
// construction) populate every field.
type Runner struct {
	// Backend is the container.Backend the run subcommand passes to
	// engine.LocalDispatcher. Production: container.NewFake() in Phase 2
	// (Phase 4 swaps to Docker). Tests: a pre-programmed *container.Fake.
	Backend container.Backend
	// IDGen mints run ids. Production: clock.CryptoIDGen{}. Tests: a
	// seeded *clock.Fake so run dir paths are reproducible.
	IDGen clock.IDGen
}

// Run is the top-level CLI entry point — constructs the production Runner
// and delegates. cmd/awf wraps the returned int with os.Exit.
func Run(args []string, stdout, stderr io.Writer) int {
	r := &Runner{
		Backend: container.NewFake(),
		IDGen:   clock.CryptoIDGen{},
	}
	return r.Run(args, stdout, stderr)
}

// Run dispatches a single CLI invocation using this Runner's dependencies.
func (r *Runner) Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "validate":
		return cliValidate(args[1:], stdout, stderr)
	case "run":
		return r.cliRun(args[1:], stdout, stderr)
	case "resume":
		return r.cliResume(args[1:], stdout, stderr)
	case "signal":
		return cliSignal(args[1:], stdout, stderr)
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
	fprintln(w, "  validate <path>           parse, validate, and print the workflow's digest")
	fprintln(w, "  run <path> [flags]        execute a workflow against the configured backend")
	fprintln(w, "                              --input <json>     run-input JSON (validated vs workflow.input)")
	fprintln(w, "                              --run-id <id>      override the minted run id")
	fprintln(w, "                              --state-dir <dir>  state directory (default: ./.awf)")
	fprintln(w, "  resume <run-id> <path>    re-enter an interrupted run against the same workflow file")
	fprintln(w, "                              --state-dir <dir>  state directory (default: ./.awf)")
	fprintln(w, "  signal <run-id> <name>    deliver a signal to an await step")
	fprintln(w, "                              --payload <json>   typed payload JSON")
	fprintln(w, "                              --state-dir <dir>  state directory")
	fprintln(w, "  help                      print this usage")
}

// fprintf / fprintln are fmt.Fprintf / fmt.Fprintln with the (n int, err error)
// return explicitly discarded. The CLI writes to bytes.Buffer (tests) or
// os.Stdout/os.Stderr (production); writes to those targets cannot fail in
// any realistic operational scenario.
func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func fprintln(w io.Writer, s string) {
	_, _ = fmt.Fprintln(w, s)
}
