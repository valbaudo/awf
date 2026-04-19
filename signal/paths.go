// Package signal is the cross-process control-surface for `awf signal` /
// `awf pause` / `awf cancel`. Per Phase 3 design decision 3, it is a per-run
// directory of JSON files; the broker is a thin file-reader/writer struct,
// NOT a generic IPC layer.
//
// Directory layout (per spec §G):
//
//	.awf/runs/<run.id>/control/
//	  signal-<name>-<seq>.json       # pending signal payloads
//	  pause.json                     # operator-requested pause (non-terminal)
//	  cancel.json                    # operator-requested cancel (terminal)
//	  consumed/
//	    signal-<name>-<seq>.json     # atomic-renamed here after consumption
//
// Slice 3.5 is POSIX-only; Windows file-lock semantics need verification
// (Phase 3 design out-of-scope note).
package signal

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ControlDir returns the control-directory path for the given run within a
// state dir. Mirrors cli/run.go's `<stateDir>/runs/<id>/log` layout pattern.
func ControlDir(stateDir, runID string) string {
	return filepath.Join(stateDir, "runs", runID, "control")
}

// ConsumedDir returns the consumed/ subdirectory within a control directory.
func ConsumedDir(controlDir string) string {
	return filepath.Join(controlDir, "consumed")
}

// signalFileName builds "signal-<name>-<seq>.json".
func signalFileName(name string, seq int) string {
	return fmt.Sprintf("signal-%s-%d.json", name, seq)
}

// parseSignalFileName returns (name, seq, ok) from a "signal-<name>-<seq>.json"
// filename. ok=false on any parse failure (caller skips the file).
func parseSignalFileName(fileName string) (name string, seq int, ok bool) {
	if !strings.HasPrefix(fileName, "signal-") {
		return "", 0, false
	}
	if !strings.HasSuffix(fileName, ".json") {
		return "", 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(fileName, "signal-"), ".json")
	idx := strings.LastIndex(mid, "-")
	if idx < 0 {
		return "", 0, false
	}
	name = mid[:idx]
	seqStr := mid[idx+1:]
	// Reject names that end with '-': "signal-name--1.json" parses via
	// LastIndex as name="name-", seqStr="1". The name part is invalid for
	// signal use (double-hyphen encodes a negative seq intent); enforce that
	// the decoded name doesn't end with the separator character.
	if len(name) == 0 || name[len(name)-1] == '-' {
		return "", 0, false
	}
	n, err := strconv.Atoi(seqStr)
	if err != nil || n < 1 {
		return "", 0, false
	}
	return name, n, true
}

const (
	pauseFileName  = "pause.json"
	cancelFileName = "cancel.json"
)

// signalNamePattern is the allowed signal-name charset (M16 fix).
// Defense-in-depth for an offensive security tool: rejects names with
// whitespace, nullbytes, path separators, and other characters that could
// confuse logging, OTel attributes (Phase 6), or shell escaping in `awf
// signal` invocations.
//
// IDENTICAL to ir/validate_structural.go's stepIDPattern. Kept as a separate
// const here to avoid creating an ir → signal package dependency for one
// regex; both patterns share the same allowed charset for consistency. If
// the two ever need to diverge, document the reason here.
var signalNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// validateSignalName returns an error if name doesn't match signalNamePattern.
// Called by Broker.WriteSignal and Broker.Receive (the CLI-facing + engine-
// facing entry points). The IR validator (ir/validate_structural.go) ALSO
// validates SignalStep.Await charset at workflow-load time; this broker-side
// check is defense-in-depth for cases where the broker is invoked directly
// (CLI args, tests).
func validateSignalName(name string) error {
	if !signalNamePattern.MatchString(name) {
		return fmt.Errorf("signal: name %q must match %s", name, signalNamePattern)
	}
	return nil
}
