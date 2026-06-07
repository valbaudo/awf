package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/runlock"
	"github.com/valbaudo/awf/state"
)

// RunRow is one entry in GET /api/runs.
type RunRow struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Workflow string `json:"workflow"`
}

// listRuns enumerates stateDir/runs and returns the runs whose workflow_digest matches
// the loaded workflow, sorted by run id. An "incomplete" run (started, no terminal
// event) is resolved via the shared runlock probe into running (a live process holds the
// lock) vs crashed (no holder), same split as `awf ls`. A missing runs dir yields an
// empty slice (not an error): a freshly-created state dir simply has no runs yet.
func listRuns(stateDir, wantDigest string) ([]RunRow, error) {
	runsDir := filepath.Join(stateDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []RunRow{}, nil
		}
		return nil, err
	}
	rows := []RunRow{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		logPath := filepath.Join(runsDir, e.Name(), "log")
		events, ferr := state.FoldFile(logPath)
		if ferr != nil {
			continue // a dir without a readable log is not a run; skip quietly
		}
		digest, wfID := runMeta(events)
		if digest != wantDigest {
			continue // a run of a different workflow (or version) -- don't offer it
		}
		status := obs.DeriveStatus(events)
		if status == obs.RunIncomplete {
			held, herr := runlock.Held(filepath.Join(runsDir, e.Name()))
			if herr != nil {
				return nil, herr
			}
			if held {
				status = obs.RunRunning
			} else {
				status = obs.RunCrashed
			}
		}
		rows = append(rows, RunRow{
			RunID:    e.Name(),
			Status:   string(status),
			Workflow: wfID,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
	return rows, nil
}

// runMeta extracts (workflow_digest, workflow_id) from a run's run.started event.
// Returns ("","") if absent.
func runMeta(events []state.Event) (digest, wfID string) {
	for _, e := range events {
		if e.Type != engine.EventRunStarted {
			continue
		}
		var d engine.RunStartedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return "", ""
		}
		return d.WorkflowDigest, d.WorkflowID
	}
	return "", ""
}
