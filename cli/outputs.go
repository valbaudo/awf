package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func printOutputsUsage(w io.Writer) {
	fprintln(w, "usage: awf outputs <run-id> [--workflow <path>] [--step <node-id>] [--dest <dir>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  read a completed run's typed outputs as JSON. Pass exactly one of:")
	fprintln(w, "  --workflow <path>  evaluate that workflow's outputs: contract (digest-checked")
	fprintln(w, "                     against the run, like `awf resume`)")
	fprintln(w, "  --step <path>     emit one committed step's typed output, read directly from")
	fprintln(w, "                    the log + blobs (no workflow file). <path> is a step's full")
	fprintln(w, "                    runtime address: a top-level id, or a nested form like")
	fprintln(w, "                    gate[0].attempt-2.generate.<id> / map[0].item-3.<id> /")
	fprintln(w, "                    loop[0].body.iter-3.<id>. Map aggregates and sub-workflow")
	fprintln(w, "                    results are read via --workflow, not --step.")
	fprintln(w, "  --dest <dir>       with --step: materialize that step's output_files into <dir>")
	fprintln(w, "                     on the host (owned by you), mirroring each declared path;")
	fprintln(w, "                     paths that would escape <dir> are refused. Prints each path.")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  Exit reflects the READ, not the run: 0 ok, 2 bad invocation, 1 read failed.")
	fprintln(w, "  Check `awf ls` or the original `awf run` exit code for run success.")
}

func cliOutputs(args []string, stdout, stderr io.Writer) int {
	fs0 := pflag.NewFlagSet("outputs", pflag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", defaultStateDir(), "base directory for runs/ and blobs/")
	step := fs0.String("step", "", "emit one committed step's typed output by runtime address")
	workflow := fs0.String("workflow", "", "workflow file: evaluate its outputs: contract")
	dest := fs0.String("dest", "", "materialize the step's output_files into this host directory (requires --step)")
	runID, code, ok := parseSinglePositional(fs0, args, "awf outputs", printOutputsUsage, stdout, stderr)
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
	if *dest != "" && *step == "" {
		fprintf(stderr, "awf outputs: --dest requires --step <node-id> (it materializes that step's output_files)\n")
		return ExitUsage
	}
	canonicalStateDir, accessErr := accessStateDir(*stateDir, stateReadOnly, defaultStateIdentity)
	if accessErr != nil {
		if errors.Is(accessErr, fs.ErrNotExist) {
			fprintf(stderr, "awf outputs: no run with id %q under state directory %q\n", runID, *stateDir)
			return ExitUsage
		}
		return reportStateFailure(stderr, "awf outputs", "access state directory", *stateDir, *stateDir, accessErr, defaultStateIdentity, stateFailureOutputs)
	}
	*stateDir = canonicalStateDir
	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf outputs: no run with id %q at %q\n", runID, logPath)
			return ExitUsage // run-not-found is a bad invocation
		}
		return reportStateFailure(stderr, "awf outputs", "fold run log", *stateDir, logPath, err, defaultStateIdentity, stateFailureOutputs)
	}
	blobs, err := state.OpenBlobsReadOnly(filepath.Join(*stateDir, "blobs"))
	if err != nil {
		return reportStateFailure(stderr, "awf outputs", "open blob store", *stateDir, filepath.Join(*stateDir, "blobs"), err, defaultStateIdentity, stateFailureOutputs)
	}

	if *step != "" {
		if *dest != "" {
			return outputsStepFiles(events, blobs, *stateDir, *step, *dest, stdout, stderr)
		}
		return outputsStep(events, blobs, *stateDir, *step, stdout, stderr)
	}
	return outputsContract(events, blobs, *stateDir, runID, *workflow, stdout, stderr)
}

// outputsStepFiles materializes a committed step's output_files onto the host,
// under dest, mirroring each declared container path (NodeCompletedData.Files is
// "declared path -> CAS ref"). The awf host process is the writer, so every file
// lands owned by the invoking user — no root-owned bind-mount writeback, no chown.
//
// Confinement is os.Root: an absolute container path (/out/x) is contained by
// stripping the leading separator (-> dest/out/x); any `..` component or symlink
// that would escape dest is refused by os.Root and reported, never followed.
// ponytail: os.Root IS the safe-join — no hand-rolled path sanitization.
func outputsStepFiles(events []state.Event, blobs state.Blobs, stateDir, nodeID, dest string, stdout, stderr io.Writer) int {
	var files map[string]string
	found := false
	// Last node.completed for this id wins (a resumed run may re-commit it),
	// mirroring outputsStep's targeted read (no full engine.Fold).
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == nodeID {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				fprintf(stderr, "awf outputs: decode node.completed at %q: %v\n", nodeID, err)
				return ExitRunFailed
			}
			files = d.Files
			found = true
		}
	}
	if !found {
		fprintf(stderr, "awf outputs: no committed step at %q (use the full runtime address, e.g. gate[0].attempt-2.generate.<id>)\n", nodeID)
		return ExitRunFailed
	}
	if len(files) == 0 {
		// Not an error: the step legitimately produced no output_files.
		fprintf(stderr, "awf outputs: step %q produced no output_files; nothing to materialize\n", nodeID)
		return ExitOK
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fprintf(stderr, "awf outputs: create dest %q: %v\n", dest, err)
		return ExitRunFailed
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		fprintf(stderr, "awf outputs: open dest %q: %v\n", dest, err)
		return ExitRunFailed
	}
	defer func() { _ = root.Close() }()

	// Deterministic order (map iteration is randomized).
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, containerPath := range paths {
		rel := strings.TrimLeft(filepath.ToSlash(containerPath), "/")
		if rel == "" || rel == "." {
			fprintf(stderr, "awf outputs: step %q has an unusable output_files path %q\n", nodeID, containerPath)
			return ExitRunFailed
		}
		raw, err := blobs.Get(files[containerPath])
		if err != nil {
			return reportStateFailure(stderr, "awf outputs", "read output_files blob for "+containerPath, stateDir, filepath.Join(stateDir, "blobs"), err, defaultStateIdentity, stateFailureOutputs)
		}
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				fprintf(stderr, "awf outputs: refuse to materialize %q outside %q: %v\n", containerPath, dest, err)
				return ExitRunFailed
			}
		}
		if err := root.WriteFile(rel, raw, 0o644); err != nil {
			fprintf(stderr, "awf outputs: refuse to materialize %q outside %q: %v\n", containerPath, dest, err)
			return ExitRunFailed
		}
		fprintln(stdout, filepath.Join(dest, rel))
	}
	return ExitOK
}

// outputsStep emits one top-level CODE/AGENT/SIGNAL step's typed output via a
// TARGETED read — scan events for its node.completed, read the single OutputsRef
// blob. Not a full engine.Fold (which errors if any UNRELATED committed blob is
// missing). Map aggregates and sub-workflow call products commit different events
// (map.item / call product) and are read via the outputs: form, not --step.
func outputsStep(events []state.Event, blobs state.Blobs, stateDir, nodeID string, stdout, stderr io.Writer) int {
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
		fprintf(stderr, "awf outputs: no committed step at %q (use the full runtime address, e.g. gate[0].attempt-2.generate.<id>; map aggregates / sub-workflow results are read via --workflow)\n", nodeID)
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
		return reportStateFailure(stderr, "awf outputs", "read output blob for "+nodeID, stateDir, filepath.Join(stateDir, "blobs"), err, defaultStateIdentity, stateFailureOutputs)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		fprintf(stderr, "awf outputs: parse output blob for %q: %v\n", nodeID, err)
		return ExitRunFailed
	}
	return emitJSON(stdout, stderr, out)
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

// outputsContract evaluates the workflow's outputs: contract against the run.
// Folds the log to a *RunState, re-loads + digest-checks the workflow (spec §8
// pinning, like awf resume), then runs the shared engine.EvaluateExportsInDef
// (def-aware so a parent {{ step.<call>.<field> }} bound to a child-omitted
// optional output omits rather than hard-fails) with a top-level scope
// (ctxPath="", input=nil → input.* resolves against the run's own input,
// matching the engine).
func outputsContract(events []state.Event, blobs state.Blobs, stateDir, runID, wfPath string, stdout, stderr io.Writer) int {
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		return reportStateFailure(stderr, "awf outputs", "read committed run state", stateDir, filepath.Join(stateDir, "blobs"), err, defaultStateIdentity, stateFailureOutputs)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		fprintf(stderr, "awf outputs: %v\n", err)
		return ExitUsage
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		fprintf(stderr, "awf outputs: workflow %q has validation errors: %v\n", wfPath, diags)
		return ExitRunFailed
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		fprintf(stderr, "awf outputs: compute digest: %v\n", err)
		return ExitUsage
	}
	if rs.WorkflowDigest != digest {
		fprintf(stderr, "awf outputs: workflow digest mismatch — run %q was started with digest %q, file %q now hashes to %q (spec §8 forbids reading outputs against a changed definition)\n", runID, rs.WorkflowDigest, wfPath, digest)
		return ExitUsage
	}
	if len(ld.Workflow.Outputs) == 0 {
		fprintf(stderr, "awf outputs: workflow %q declares no outputs:; use --step <node-id>\n", wfPath)
		return ExitUsage
	}
	res, err := engine.EvaluateExportsInDef(ld, "", rs, ld.Workflow, "", nil, blobs)
	if err != nil {
		// A referenced step did not commit (skipped or never ran), or a schema
		// mismatch — a data condition (the run did not produce this output),
		// distinct from a usage error. See spec §2.4 (run-success != output-success).
		return reportStateFailure(stderr, "awf outputs", "evaluate committed outputs", stateDir, filepath.Join(stateDir, "blobs"), err, defaultStateIdentity, stateFailureOutputs)
	}
	return emitJSON(stdout, stderr, res.Outputs)
}
