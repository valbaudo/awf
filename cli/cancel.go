package cli

import (
	"errors"
	"io"

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

func cliCancel(args []string, stdout, stderr io.Writer) int {
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
	if rc := requireRunDir(*stateDir, runID, stderr); rc != ExitOK {
		return rc
	}
	broker := signal.NewBroker(signal.ControlDir(*stateDir, runID))
	if err := broker.WriteCancel(signal.CancelRequest{Reason: *reason}); err != nil {
		fprintf(stderr, "awf cancel: %v\n", err)
		return ExitUsage
	}
	fprintf(stdout, "cancel requested for run %s\n", runID)
	return ExitOK
}
