package ui

import (
	"encoding/json"
	"errors"
	"io/fs"
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
	// VersionMatch is true when the run's recorded definition digest equals the digest of the
	// workflow file the UI was launched against — i.e. the run executed against the current
	// version. When false, the run is an earlier (or later) version of the same workflow; the
	// SPA badges it. The run still renders faithfully against its own snapshot.
	VersionMatch bool `json:"version_match"`
}

// listRuns enumerates stateDir/runs and returns the runs belonging to the loaded workflow, sorted
// by run id. Scoping is by workflow id (the stable `workflow:` identifier), so every version of the
// same workflow is listed even after the file is edited — not just the exact-digest match. An
// "incomplete" run (started, no terminal event) is resolved via the shared runlock probe into
// running (a live process holds the lock) vs crashed (no holder), same split as `awf ls`. A missing
// runs dir yields an empty slice (not an error): a freshly-created state dir simply has no runs yet.
func listRuns(stateDir, wantID, wantDigest string) ([]RunRow, error) {
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
			if errors.Is(ferr, fs.ErrNotExist) {
				continue // an unrelated directory without a log is not a run
			}
			return nil, ferr
		}
		digest, wfID := runMeta(events)
		if !runMatchesWorkflow(wfID, digest, wantID, wantDigest) {
			continue // a run of a different workflow -- don't offer it
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
			RunID:        e.Name(),
			Status:       string(status),
			Workflow:     wfID,
			VersionMatch: digest == wantDigest,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
	return rows, nil
}

// runMatchesWorkflow reports whether a run belongs to the workflow the UI was launched against.
// The primary key is the workflow id (stable across edits) so every version of the same workflow is
// listed. Pre-6.1 logs have no workflow id; for those we fall back to the exact digest, preserving
// the old "same file only" behaviour for legacy runs.
func runMatchesWorkflow(wfID, digest, wantID, wantDigest string) bool {
	if wfID != "" {
		return wfID == wantID
	}
	return digest == wantDigest
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

// runDefinitionRef extracts the run's definition snapshot ref (run.started.definition_ref), or ""
// if absent (pre-snapshot logs) or unreadable.
func runDefinitionRef(events []state.Event) string {
	for _, e := range events {
		if e.Type != engine.EventRunStarted {
			continue
		}
		var d engine.RunStartedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return ""
		}
		return d.DefinitionRef
	}
	return ""
}
