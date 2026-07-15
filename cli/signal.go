package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/signal"
)

func printSignalUsage(w io.Writer) {
	fprintln(w, "usage: awf signal <run-id> <name> [--payload <json>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  deliver a signal to a running (or future) await step. Writes a control file")
	fprintln(w, "  in <state-dir>/runs/<run-id>/control/; the engine consumes it at the next")
	fprintln(w, "  await wake-up. If the run is not yet started, the file persists and the")
	fprintln(w, "  signal is consumed when the run reaches the matching await.")
	fprintln(w, "")
	fprintln(w, "  --payload <json>   typed payload (validated by the await step's output_schema)")
	fprintln(w, "  --state-dir <dir>  state directory (default: ./.awf)")
}

func cliSignalWithIdentity(args []string, stdout, stderr io.Writer, lookup stateIdentityLookup) int {
	fs0 := pflag.NewFlagSet("signal", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	payload := fs0.String("payload", "", "typed payload JSON")
	stateDir := fs0.String("state-dir", defaultStateDir(), "base directory for runs/ and blobs/")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			printSignalUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf signal: %v\n", err)
		printSignalUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 2 {
		printSignalUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	name := fs0.Arg(1)

	// Validate --payload is well-formed JSON if non-empty (the await step's
	// output_schema validation happens engine-side, but a malformed JSON
	// payload would silently fail there; surface it here for a clearer error).
	var payloadBytes []byte
	if *payload != "" {
		var probe any
		if err := json.Unmarshal([]byte(*payload), &probe); err != nil {
			fprintf(stderr, "awf signal: --payload is not valid JSON: %v\n", err)
			return ExitUsage
		}
		payloadBytes = []byte(*payload)
	}
	canonicalStateDir, accessErr := accessStateDir(*stateDir, stateWriteExisting, lookup)
	if accessErr != nil {
		if errors.Is(accessErr, fs.ErrNotExist) {
			return requireRunDir(*stateDir, runID, stderr)
		}
		return reportStateFailure(stderr, "awf signal", "access state directory", *stateDir, *stateDir, accessErr, lookup, stateFailureInfra)
	}
	*stateDir = canonicalStateDir

	// Refuse if run dir doesn't exist (defense against typo'd run-ids that
	// would create an orphan control dir).
	if rc := requireRunDirForCommand(*stateDir, runID, stderr, "awf signal", lookup); rc != ExitOK {
		return rc
	}

	controlDir := signal.ControlDir(*stateDir, runID)
	broker := signal.NewBroker(controlDir)
	seq, err := broker.WriteSignal(name, payloadBytes)
	if err != nil {
		return reportStateFailure(stderr, "awf signal", "write signal control file", *stateDir, controlDir, err, lookup, stateFailureInfra)
	}
	fprintf(stdout, "signal %s (seq %d) written to %s\n", name, seq, controlDir)
	return ExitOK
}
