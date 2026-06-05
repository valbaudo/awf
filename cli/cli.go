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

	"github.com/valbaudo/awf/agent"
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
	// Resolver is the agent.Adapter registry the CLI uses to look up `uses:`
	// refs at run-start (Task 12) and resume (Task 14). Test-injection point:
	// tests assign a *agent.Registry populated with agent/fake adapters and
	// drive workflows that exercise the version-pinning + drift-check paths.
	//
	// In slice 5.1, this field is the ONLY way a real adapter could reach the
	// CLI — there's no production wiring that constructs one. The empty
	// fallback (via resolverOrEmpty(), Task 12) means any workflow using a
	// `uses:` step fails at run-start with *ErrAdapterNotFound. That's the
	// correct behavior for slice 5.1: no real adapter ships until slice 5.3
	// (cli/agent_registry.go), at which point cli/run.go grows production
	// code that constructs and assigns *agent.Registry into this field
	// before dispatching engine.Run.
	//
	// Workflows that don't reference any `uses:` step (every Phase 2-4
	// fixture) run unaffected — they hit zero Lookup calls regardless of
	// what Resolver is.
	Resolver agent.Resolver
	// AgentEnv is the env-var allowlist the CLI uses to construct the
	// production *agent.Registry (when Resolver is nil). Slice 5.3
	// test-injection point: tests assign []string{"ANTHROPIC_API_KEY"} to
	// drive cli/agent_registry.go's buildAgentRegistry without the
	// os.Setenv dance that --agent-env-via-flag would require.
	//
	// When Resolver is non-nil, AgentEnv is ignored (the test has injected
	// a fully-constructed Registry, bypassing buildAgentRegistry).
	// Production: cli/run.go reads --agent-env, splits to []string, and
	// passes here BEFORE calling buildAgentRegistry.
	AgentEnv []string
	// AgentEventTap is the io.Writer agent.AgentEvent live-tap lines are written
	// to during dispatch. Test-injection point: tests assign a bytes.Buffer to
	// capture the rendered lines, or io.Discard to silence. Production wiring
	// in cli/execute.go (Task 11) defaults to stderr.
	//
	// nil disables the live tap entirely (the dispatcher's writeAgentEventTap
	// becomes a no-op). Workflows without any `uses:` step (Phase 2-4
	// fixtures) are unaffected — the tap never fires.
	AgentEventTap io.Writer
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
	case "ls":
		return cliLS(args[1:], stdout, stderr)
	case "inspect":
		return cliInspect(args[1:], stdout, stderr)
	case "trace":
		return cliTrace(args[1:], stdout, stderr)
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
	fprintln(w, "                              --backend <kind>   container backend: fake|docker|native (default: native)")
	fprintln(w, "                              --agent-env <csv>  env-var name allowlist forwarded to agent CLIs")
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
	fprintln(w, "  ls                        list runs and their derived status")
	fprintln(w, "                              --state-dir <dir>  state directory")
	fprintln(w, "                              --output <fmt>     text (default) or json")
	fprintln(w, "  inspect <run-id>          fold-by-status text tree of a run's addressing tree")
	fprintln(w, "                              --fold <statuses>  outcomes to collapse (default: ok)")
	fprintln(w, "                              --depth <n>        max tree depth")
	fprintln(w, "                              --output <fmt>     text (default) or json")
	fprintln(w, "                              --tokens           per-step input/output token counts")
	fprintln(w, "                              --state-dir <dir>  state directory")
	fprintln(w, "  trace <run-id>            export a run's OTel span projection")
	fprintln(w, "                              --otlp <endpoint>  export to an OTLP/HTTP collector")
	fprintln(w, "                              --capture-content  attach agent I/O + outputs (default OFF)")
	fprintln(w, "                              --output <fmt>     otel (default) or json")
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
