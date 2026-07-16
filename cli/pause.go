package cli

import (
	"errors"
	"io"
	"io/fs"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/signal"
)

func printPauseUsage(w io.Writer) {
	fprintln(w, "usage: awf pause <run-id> [--reason <text>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  halt dispatch at the next commit boundary. Containers stay up so an operator")
	fprintln(w, "  can inspect the live workspace and committed artifacts; `awf resume` continues")
	fprintln(w, "  in a new epoch. Non-terminal — pause does NOT prevent later resume.")
	fprintln(w, "")
	fprintln(w, "  --reason <text>       operator-supplied free-text reason")
	fprintln(w, "  --state-dir <dir>     state directory (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  note: --before <node-path> (per awf-workflow(5)) is reserved")
	fprintln(w, "  for Phase 6's obs subsystem and is rejected in Phase 3. Pause currently")
	fprintln(w, "  halts at the next commit boundary only.")
}

func cliPauseWithIdentity(args []string, stdout, stderr io.Writer, lookup stateIdentityLookup) int {
	fs0 := pflag.NewFlagSet("pause", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	// --before is accepted for parsing-compat (so users get a clear error
	// instead of "unknown flag") but rejected at runtime. H8 fix: better to
	// reject explicitly than silently ignore (kubectl convention).
	before := fs0.String("before", "", "node-path breakpoint (not yet supported)")
	reason := fs0.String("reason", "", "free-text reason")
	stateDir := fs0.String("state-dir", defaultStateDir(), "state directory")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			printPauseUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf pause: %v\n", err)
		printPauseUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 1 {
		printPauseUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	// H8 fix: reject --before explicitly. Phase 6 obs ships the breakpoint
	// mechanism (watches commits in real time); Phase 3 has no machinery to
	// check "execution reached node path X." Silent accept-and-ignore would
	// be UX drift from awf-workflow(5) (CHECKPOINTING AND RESUME).
	if *before != "" {
		fprintf(stderr, "awf pause: --before <node-path> is not yet supported in Phase 3 (lands with Phase 6 obs). Drop the flag to pause at the next commit boundary.\n")
		return ExitUsage
	}
	canonicalStateDir, accessErr := accessStateDir(*stateDir, stateWriteExisting, lookup)
	if accessErr != nil {
		if errors.Is(accessErr, fs.ErrNotExist) {
			return requireRunDir(*stateDir, runID, stderr)
		}
		return reportStateFailure(stderr, "awf pause", "access state directory", *stateDir, *stateDir, accessErr, lookup, stateFailureInfra)
	}
	*stateDir = canonicalStateDir
	if rc := requireRunDirForCommand(*stateDir, runID, stderr, "awf pause", lookup); rc != ExitOK {
		return rc
	}
	broker := signal.NewBroker(signal.ControlDir(*stateDir, runID))
	if err := broker.WritePause(signal.PauseRequest{Reason: *reason}); err != nil {
		return reportStateFailure(stderr, "awf pause", "write pause control file", *stateDir, signal.ControlDir(*stateDir, runID), err, lookup, stateFailureInfra)
	}
	fprintf(stdout, "pause requested for run %s\n", runID)
	return ExitOK
}
