package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func printOutputsUsage(w io.Writer) {
	fprintln(w, "usage: awf outputs <run-id> [--workflow <path>] [--step <node-id>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  read a completed run's typed outputs as JSON. Pass exactly one of:")
	fprintln(w, "  --workflow <path>  evaluate that workflow's outputs: contract (digest-checked")
	fprintln(w, "                     against the run, like `awf resume`)")
	fprintln(w, "  --step <node-id>   emit one top-level code/agent step's typed output (no")
	fprintln(w, "                     workflow file needed). Map aggregates and sub-workflow")
	fprintln(w, "                     results are read via --workflow, not --step.")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  Exit reflects the READ, not the run: 0 ok, 2 bad invocation, 1 read failed.")
	fprintln(w, "  Check `awf ls` or the original `awf run` exit code for run success.")
}

func cliOutputs(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("outputs", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	step := fs0.String("step", "", "emit one top-level code/agent step's typed output")
	workflow := fs0.String("workflow", "", "workflow file: evaluate its outputs: contract")
	runID, code, ok := parseRunIDFirst(fs0, args, "awf outputs", printOutputsUsage, stdout, stderr)
	if !ok {
		return code
	}

	if *step != "" && *workflow != "" {
		fprintf(stderr, "awf outputs: use either --step (log-only) or --workflow (the outputs: contract), not both\n")
		return ExitUsage
	}
	if *step == "" && *workflow == "" {
		fprintf(stderr, "awf outputs: provide --workflow <path> (to read the outputs: contract) or --step <node-id>\n")
		return ExitUsage
	}
	if *step != "" && isRuntimeSuffixedPath(*step) {
		fprintf(stderr, "awf outputs: %q is a runtime-internal path; P1 reads top-level node ids. Use `awf inspect`/`awf trace` for gate/map-internal outputs.\n", *step)
		return ExitUsage
	}

	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf outputs: no run with id %q at %q\n", runID, logPath)
			return ExitUsage // run-not-found is a bad invocation
		}
		fprintf(stderr, "awf outputs: fold log %q: %v\n", logPath, err)
		return ExitRunFailed // corrupt/unreadable log is a read failure
	}
	blobs, err := state.OpenBlobs(filepath.Join(*stateDir, "blobs"))
	if err != nil {
		fprintf(stderr, "awf outputs: open blobs: %v\n", err)
		return ExitRunFailed // blob-store open failure is a read-infra failure
	}

	if *step != "" {
		return outputsStep(events, blobs, *step, stdout, stderr)
	}
	return outputsContract(events, blobs, runID, *workflow, stdout, stderr)
}

// outputsStep emits one top-level CODE/AGENT/SIGNAL step's typed output via a
// TARGETED read — scan events for its node.completed, read the single OutputsRef
// blob. Not a full engine.Fold (which errors if any UNRELATED committed blob is
// missing). Map aggregates and sub-workflow call products commit different events
// (map.item / call product) and are read via the outputs: form, not --step.
func outputsStep(events []state.Event, blobs state.Blobs, nodeID string, stdout, stderr io.Writer) int {
	if isRuntimeSuffixedPath(nodeID) {
		fprintf(stderr, "awf outputs: %q is a runtime-internal path; P1 reads top-level node ids. Use `awf inspect`/`awf trace` for gate/map-internal outputs.\n", nodeID)
		return ExitUsage
	}
	ref := ""
	found := false
	// Last node.completed for this id wins (a resumed run may re-commit it).
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == nodeID {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				fprintf(stderr, "awf outputs: decode node.completed at %q: %v\n", nodeID, err)
				return ExitRunFailed
			}
			ref = d.OutputsRef
			found = true
		}
	}
	if !found {
		fprintf(stderr, "awf outputs: no committed step at %q (map aggregates / sub-workflow results are read via --workflow, not --step)\n", nodeID)
		return ExitRunFailed
	}
	if ref == "" {
		fprintf(stderr, "awf outputs: step %q has no typed output (no output_schema)\n", nodeID)
		return ExitRunFailed
	}
	// blobs.Get buffers the whole blob; typed outputs are conventionally small
	// (there is no enforced cap) so buffering is acceptable for a read command.
	raw, err := blobs.Get(ref)
	if err != nil {
		fprintf(stderr, "awf outputs: read output blob for %q: %v\n", nodeID, err)
		return ExitRunFailed
	}
	var outMap map[string]any
	if err := json.Unmarshal(raw, &outMap); err != nil {
		fprintf(stderr, "awf outputs: parse output blob for %q: %v\n", nodeID, err)
		return ExitRunFailed
	}
	return emitJSON(stdout, stderr, outMap)
}

// isRuntimeSuffixedPath rejects gate/map-internal runtime paths: only top-level
// node ids are addressable in P1.
func isRuntimeSuffixedPath(p string) bool {
	return strings.Contains(p, "[") ||
		strings.Contains(p, ".iter-") ||
		strings.Contains(p, ".attempt-") ||
		strings.Contains(p, ".item-")
}

// emitJSON pretty-prints v (matching inspect/trace/ls/graph: NewEncoder +
// two-space indent + trailing newline).
func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fprintf(stderr, "awf outputs: json encode: %v\n", err)
		return ExitRunFailed
	}
	return ExitOK
}

// outputsContract is implemented in Task 3 (which adds the loader/ir imports with
// the real body). Stub keeps the file compiling — unused params use `_` so lint
// stays clean; Task 3 names them.
func outputsContract(_ []state.Event, _ state.Blobs, _, _ string, _, stderr io.Writer) int {
	fprintf(stderr, "awf outputs: outputs: form not yet implemented\n")
	return ExitUsage
}
