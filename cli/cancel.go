package cli

import (
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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

func cliCancel(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	reason := fs0.String("reason", "", "free-text reason")
	stateDir := fs0.String("state-dir", ".awf", "state directory")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCancelUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf cancel: %v\n", err)
		printCancelUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() < 1 {
		printCancelUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	// Re-parse any trailing flags after the positional <run-id>. Go's flag
	// package stops at the first non-flag token, so flags like --reason that
	// appear after the run-id require a second pass.
	if fs0.NArg() > 1 {
		if err := fs0.Parse(fs0.Args()[1:]); err != nil {
			fprintf(stderr, "awf cancel: %v\n", err)
			printCancelUsage(stderr)
			return ExitUsage
		}
		if fs0.NArg() != 0 {
			printCancelUsage(stderr)
			return ExitUsage
		}
	}
	runDir := filepath.Join(*stateDir, "runs", runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf cancel: no run with id %q at %q\n", runID, runDir)
		} else {
			fprintf(stderr, "awf cancel: stat run dir %q: %v\n", runDir, err)
		}
		return ExitUsage
	}
	broker := signal.NewBroker(signal.ControlDir(*stateDir, runID))
	if err := broker.WriteCancel(signal.CancelRequest{Reason: *reason}); err != nil {
		fprintf(stderr, "awf cancel: %v\n", err)
		return ExitUsage
	}
	fprintf(stdout, "cancel requested for run %s\n", runID)
	return ExitOK
}
