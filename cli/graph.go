package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func printGraphUsage(w io.Writer) {
	fprintln(w, "usage: awf graph <path> [--run <id>] [--state-dir <dir>] [--output json]")
	fprintln(w, "")
	fprintln(w, "  emit the workflow's node/edge graph as JSON (the visual-graph contract).")
	fprintln(w, "  without --run, emits the static template graph.")
	fprintln(w, "  with --run <id>, attaches a flat run_overlay (per-path run state).")
	fprintln(w, "  --run <id>         overlay state from this run (omit for static graph)")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ (default: ./.awf)")
	fprintln(w, "  --output <fmt>     output format: json (default)")
	fprintln(w, "")
	fprintln(w, "  NOTE: run ids are explicit; there is no --run=latest (run ids are random")
	fprintln(w, "  and not time-ordered on disk — see `awf ls`).")
}

// cliGraph runs `awf graph <path> [--run <id>] [--state-dir <dir>] [--output json]`.
//
//	ExitOK    — projection emitted (static, or static + overlay).
//	ExitUsage — bad args, unreadable/invalid workflow, unknown run id, or fold/encode failure.
//
// No --run emits the static graph (exit 0). --run=<id> against a never-run id is an
// error. There is no --run=latest: run ids are random and not time-ordered on disk.
func cliGraph(args []string, stdout, stderr io.Writer) int {
	fs0 := pflag.NewFlagSet("graph", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	runID := fs0.String("run", "", "run id to overlay state from (omit for static graph)")
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/")
	output := fs0.StringP("output", "o", "json", "output format: json")
	// Path is the first positional, then flags (e.g. `awf graph wf.yaml --run r1`).
	// parseSinglePositional returns that first positional (here, the workflow path).
	path, code, ok := parseSinglePositional(fs0, args, "awf graph", printGraphUsage, stdout, stderr)
	if !ok {
		return code
	}
	if *output != "json" {
		fprintf(stderr, "awf graph: unknown --output %q (want json)\n", *output)
		return ExitUsage
	}

	ld, loadErr := loader.Load(path)
	if loadErr != nil {
		fprintf(stderr, "awf graph: %v\n", loadErr)
		return ExitUsage
	}

	proj := graph.BuildStaticLoaded(ld)

	if *runID != "" {
		logPath := filepath.Join(*stateDir, "runs", *runID, "log")
		events, foldErr := state.FoldFile(logPath)
		if foldErr != nil {
			if errors.Is(foldErr, fs.ErrNotExist) {
				fprintf(stderr, "awf graph: no run with id %q at %q\n", *runID, logPath)
			} else {
				fprintf(stderr, "awf graph: fold log %q: %v\n", logPath, foldErr)
			}
			return ExitUsage
		}
		runProj, projErr := graph.BuildWithRunLoaded(ld, events)
		if projErr != nil {
			fprintf(stderr, "awf graph: project log: %v\n", projErr)
			return ExitUsage
		}
		proj = runProj
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(proj); err != nil {
		fprintf(stderr, "awf graph: json encode: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}
