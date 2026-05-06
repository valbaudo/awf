package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func printLSUsage(w io.Writer) {
	fprintln(w, "usage: awf ls [--state-dir <dir>] [--output text|json]")
	fprintln(w, "")
	fprintln(w, "  list runs under <state-dir>/runs/ with a derived status:")
	fprintln(w, "  running / paused / finished / failed / cancelled / crashed.")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
	fprintln(w, "  --output <fmt>     text (default) or json")
}

// lsRow is one run's row (the --output json schema).
type lsRow struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Workflow string `json:"workflow,omitempty"`
}

func cliLS(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	output := fs0.String("output", "text", "output format: text or json")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printLSUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf ls: %v\n", err)
		printLSUsage(stderr)
		return ExitUsage
	}
	if *output != "text" && *output != "json" {
		fprintf(stderr, "awf ls: unknown --output %q (want text or json)\n", *output)
		return ExitUsage
	}

	runsDir := filepath.Join(*stateDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emitLS(stdout, *output, nil)
		}
		fprintf(stderr, "awf ls: read runs dir %q: %v\n", runsDir, err)
		return ExitUsage
	}

	var rows []lsRow
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runDir := filepath.Join(runsDir, e.Name())
		logPath := filepath.Join(runDir, "log")
		events, ferr := state.FoldFile(logPath)
		if ferr != nil {
			// A directory without a readable log is not a run; skip quietly.
			continue
		}
		status := obs.DeriveStatus(events)
		if status == obs.RunIncomplete {
			held, herr := runLockHeld(runDir)
			if herr != nil {
				fprintf(stderr, "awf ls: probe liveness for %q: %v\n", e.Name(), herr)
				return ExitUsage
			}
			if held {
				status = obs.RunRunning
			} else {
				status = obs.RunCrashed
			}
		}
		rows = append(rows, lsRow{RunID: e.Name(), Status: string(status), Workflow: workflowIDFromEvents(events)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
	return emitLS(stdout, *output, rows)
}

// workflowIDFromEvents pulls the workflow id off run.started (best-effort; "").
// Decodes the real engine.RunStartedData (no ad-hoc struct) and matches the
// engine.EventRunStarted constant (no magic string).
func workflowIDFromEvents(events []state.Event) string {
	for _, e := range events {
		if e.Type == engine.EventRunStarted {
			var d engine.RunStartedData
			_ = json.Unmarshal(e.Data, &d)
			return d.WorkflowID
		}
	}
	return ""
}

func emitLS(stdout io.Writer, output string, rows []lsRow) int {
	if output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []lsRow{}
		}
		if err := enc.Encode(rows); err != nil {
			return ExitUsage
		}
		return ExitOK
	}
	for _, r := range rows {
		if r.Workflow != "" {
			fprintf(stdout, "%-24s  %-9s  %s\n", r.RunID, r.Status, r.Workflow)
		} else {
			fprintf(stdout, "%-24s  %s\n", r.RunID, r.Status)
		}
	}
	return ExitOK
}
