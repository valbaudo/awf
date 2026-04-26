// Package cli assembles the command-line surface. Slice 1.6 shipped
// `awf validate <path>`; slice 2.5 added `awf run`; slice 2.6 added
// `awf resume`; Phase 3 added `signal` / `pause` / `cancel`; slice 4.5
// added `--backend {fake,docker,native}` on `awf run` (default native; slice 4.7) +
// log-driven backend selection on `awf resume`. Later phases add
// `inspect` / `trace` / `ls` (Phase 6). The entry point is Runner.Run,
// not init() or a package-level CLI framework, so tests drive the full
// surface with bytes.Buffer for IO and an int return for the exit code
// — no real os.Exit ever called from package code.
//
// Runner holds the production-vs-test seams: Backend (test injection;
// production leaves it nil and the run/resume subcommands construct via
// the package-private newBackend helper when Backend is nil — see
// cli/backend.go for the kind switch) and IDGen.
package cli

import (
	"fmt"
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/signal"
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
	// engine.LocalDispatcher. Production: left nil — the run/resume
	// subcommands construct on-demand via newBackend (cli/backend.go),
	// choosing fake/docker/native based on the --backend flag (default
	// native; slice 4.7). Tests: inject any container.Backend (typically
	// a pre-programmed *container.Fake) to short-circuit construction
	// via (*Runner).resolveBackend.
	Backend container.Backend
	// IDGen mints run ids. Production: clock.CryptoIDGen{}. Tests: a
	// seeded *clock.Fake so run dir paths are reproducible.
	IDGen clock.IDGen
	// BrokerOptions are passed to signal.NewBroker when the run/resume
	// subcommands construct a broker. Slice 3.5 test-injection hook: tests
	// pass signal.WithPollInterval(time.Millisecond) for fast polling; the
	// production cli.Run constructor leaves this nil (defaults to 100ms).
	BrokerOptions []signal.BrokerOption
}

// Run is the top-level CLI entry point — constructs the production Runner
// and delegates. cmd/awf wraps the returned int with os.Exit.
//
// Slice 4.5: Backend is intentionally left nil; the run / resume
// subcommands construct it on-demand via newBackend (chosen by --backend
// flag on run; read from the log's run.started.Backend on resume). This
// defers docker-client construction to the moment we know the user
// actually wants Docker — important because a docker.New failure (Docker
// daemon not running) shouldn't crash `awf validate` or `awf help`.
func Run(args []string, stdout, stderr io.Writer) int {
	r := &Runner{
		IDGen: clock.CryptoIDGen{},
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
	case "pause":
		return cliPause(args[1:], stdout, stderr)
	case "cancel":
		return cliCancel(args[1:], stdout, stderr)
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
	fprintln(w, "  pause <run-id>            halt at next commit boundary (non-terminal)")
	fprintln(w, "                              --reason <text>    operator-supplied reason")
	fprintln(w, "                              --state-dir <dir>  state directory")
	fprintln(w, "  cancel <run-id>           TERMINAL cancel; `awf resume` refuses afterwards")
	fprintln(w, "                              --reason <text>    operator-supplied reason")
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
