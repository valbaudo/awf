package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/pflag"

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

// lsRow is one run's row (the --output json schema). CostUSD/InputTokens/
// OutputTokens are pointers so absent (no agent step, or no reported cost) is
// distinguishable from a real zero — a $0.0000 on an unpriced adapter would
// read as "free", which it isn't. omitempty keeps code-only runs byte-identical
// to the pre-metrics schema.
// UnpricedSteps (F35 roll-up parity) surfaces the same signal
// printRunCostSummary shows: nil/omitted when every priced agent step
// reported a cost source, so a fully-priced run's JSON is unchanged; a
// non-nil count means CostUSD is a floor, not a complete total.
type lsRow struct {
	RunID         string   `json:"run_id"`
	Status        string   `json:"status"`
	Workflow      string   `json:"workflow,omitempty"`
	CostUSD       *float64 `json:"cost,omitempty"`
	InputTokens   *int     `json:"input_tokens,omitempty"`
	OutputTokens  *int     `json:"output_tokens,omitempty"`
	UnpricedSteps *int     `json:"unpriced_steps,omitempty"`
}

func cliLS(args []string, stdout, stderr io.Writer) int {
	fs0 := pflag.NewFlagSet("ls", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", defaultStateDir(), "base directory for runs/ and blobs/")
	output := fs0.StringP("output", "o", "text", "output format: text or json")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			printLSUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf ls: %v\n", err)
		printLSUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 0 {
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
		return ExitInfra
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
				return ExitInfra
			}
			if held {
				status = obs.RunRunning
			} else {
				status = obs.RunCrashed
			}
		}
		row := lsRow{RunID: e.Name(), Status: string(status), Workflow: workflowIDFromEvents(events)}
		// Reuse the same folded events for the cost/token rollup — no extra read.
		if m := foldRunMetrics(events); m.AgentSteps > 0 {
			in, out := m.InTok, m.OutTok
			row.InputTokens, row.OutputTokens = &in, &out
			if m.HasCost {
				usd := m.TotalUSD
				row.CostUSD = &usd
			}
			if m.UnpricedSteps > 0 {
				up := m.UnpricedSteps
				row.UnpricedSteps = &up
			}
		}
		rows = append(rows, row)
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
			fprintf(stdout, "%-24s  %-9s  %-16s%s\n", r.RunID, r.Status, r.Workflow, lsMetricsSuffix(r))
		} else {
			fprintf(stdout, "%-24s  %-9s%s\n", r.RunID, r.Status, lsMetricsSuffix(r))
		}
	}
	return ExitOK
}

// lsMetricsSuffix renders the trailing cost/token columns for the text view.
// Empty for runs with no agent steps. Cost shows "—" (not "$0.0000") when no
// adapter reported a cost, so an unpriced run never reads as free. Tokens are
// always input/output split — a combined total hides the in-vs-out ratio.
// F35 parity: when some (but not all) agent steps reported no cost source,
// the dollar figure is a floor, not a complete total — append the same "+"
// printRunCostSummary uses, so a mixed priced/unpriced run doesn't read as
// complete here either. A fully-priced run (UnpricedSteps nil) is unchanged.
func lsMetricsSuffix(r lsRow) string {
	if r.InputTokens == nil {
		return ""
	}
	cost := "—"
	if r.CostUSD != nil {
		cost = formatUSD(*r.CostUSD)
		if r.UnpricedSteps != nil {
			cost += "+"
		}
	}
	return fmt.Sprintf("  %-9s  %d in / %d out tok", cost, *r.InputTokens, *r.OutputTokens)
}
