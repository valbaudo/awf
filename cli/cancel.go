package cli

import (
	"errors"
	"io"
	"io/fs"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/signal"
)

func printCancelUsage(w io.Writer) {
	fprintln(w, "usage: awf cancel <run-id> [--reason <text>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  TERMINAL cancel. The engine cancels its root ctx (in-flight steps see")
	fprintln(w, "  ctx.Done()), runs `finally` blocks, tears down containers, and appends a")
	fprintln(w, "  terminal run.cancelled event. `awf resume` refuses (4th refusal class).")
	fprintln(w, "")
	fprintln(w, "  --reason <text>    operator-supplied free-text reason")
	fprintln(w, "  --state-dir <dir>  state directory (default: ./.awf)")
}

func cliCancelWithIdentity(args []string, stdout, stderr io.Writer, lookup stateIdentityLookup) int {
	fs0 := pflag.NewFlagSet("cancel", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	reason := fs0.String("reason", "", "free-text reason")
	stateDir := fs0.String("state-dir", defaultStateDir(), "state directory")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			printCancelUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf cancel: %v\n", err)
		printCancelUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 1 {
		printCancelUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	canonicalStateDir, accessErr := accessStateDir(*stateDir, stateWriteExisting, lookup)
	if accessErr != nil {
		if errors.Is(accessErr, fs.ErrNotExist) {
			return requireRunDir(*stateDir, runID, stderr)
		}
		return reportStateFailure(stderr, "awf cancel", "access state directory", *stateDir, *stateDir, accessErr, lookup, stateFailureInfra)
	}
	*stateDir = canonicalStateDir
	if rc := requireRunDirForCommand(*stateDir, runID, stderr, "awf cancel", lookup); rc != ExitOK {
		return rc
	}
	broker := signal.NewBroker(signal.ControlDir(*stateDir, runID))
	if err := broker.WriteCancel(signal.CancelRequest{Reason: *reason}); err != nil {
		return reportStateFailure(stderr, "awf cancel", "write cancel control file", *stateDir, signal.ControlDir(*stateDir, runID), err, lookup, stateFailureInfra)
	}
	fprintf(stdout, "cancel requested for run %s\n", runID)
	return ExitOK
}
