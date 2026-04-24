package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	awfsignal "github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// printResumeUsage writes the resume-subcommand usage line.
func printResumeUsage(w io.Writer) {
	fprintln(w, "usage: awf resume <run-id> <path> [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  re-enter an interrupted run. The workflow file at <path> must hash to the")
	fprintln(w, "  same digest as the run's original definition (spec §8 pinning); a mismatch is a hard")
	fprintln(w, "  error. A run that already finished (run.finished in the log) or terminated on a")
	fprintln(w, "  failed step (node.failed in the log) cannot be resumed.")
	fprintln(w, "")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
}

// cliResume implements `awf resume <run-id> <path>`. The flow:
//
//  1. Parse flags + positional args.
//  2. Open the existing log (NOT exclusive — this is the resume primitive).
//  3. Open blobs; fold the log into a populated RunState.
//  4. Refuse if a terminal event is in the log: run.finished (run is
//     complete) or node.failed (Phase 2 has no try/catch; resuming would
//     re-execute the failed step).
//  5. Load + validate + digest the workflow file.
//  6. Refuse on digest mismatch (spec §8 hard error).
//  7. Signal context + Create container handles per the workflow's
//     containers map (CLI-owned lifecycle, slice 2.5 Design question 3).
//  8. Append run.resumed{epoch: rs.Epoch+1} + Sync.
//  9. runAndFinish: build dispatcher, engine.Run, append run.finished,
//     map outcome → exit code (shared with `awf run` — cli/execute.go).
func (r *Runner) cliResume(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printResumeUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf resume: %v\n", err)
		printResumeUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 2 {
		printResumeUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	wfPath := fs0.Arg(1)

	// Step 1: open the existing log. errors.Is(err, fs.ErrNotExist) means
	// unknown run id — distinct from a generic I/O error so the user gets a
	// useful message.
	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	log, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf resume: no run with id %q at %q. Did you mean a different --state-dir?\n", runID, logPath)
		} else {
			fprintf(stderr, "awf resume: open log %q: %v\n", logPath, err)
		}
		return ExitUsage
	}
	defer func() { _ = log.Close() }()

	// Step 2: fold the log into a populated RunState. The blobs need to be
	// available so engine.Fold can resolve OutputsRef / StdoutRef / InputRef.
	blobsDir := filepath.Join(*stateDir, "blobs")
	blobs, err := state.OpenBlobs(blobsDir)
	if err != nil {
		fprintf(stderr, "awf resume: open blobs %q: %v\n", blobsDir, err)
		return ExitUsage
	}
	events, err := log.Fold()
	if err != nil {
		fprintf(stderr, "awf resume: fold log: %v\n", err)
		return ExitUsage
	}
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		fprintf(stderr, "awf resume: build RunState: %v\n", err)
		return ExitUsage
	}

	// Step 3: refusal — run.finished already in the log (terminal).
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			fprintf(stderr, "awf resume: run %q already finished (run.finished event in log). Cannot resume a completed run.\n", runID)
			return ExitUsage
		}
	}
	// Step 3b (slice 3.5): refusal — run.cancelled already in log (terminal).
	// Checked BEFORE node.failed: cancel-during-step writes both events; the
	// user wants to see "cancelled," not "failed step."
	for _, e := range events {
		if e.Type == engine.EventRunCancelled {
			fprintf(stderr, "awf resume: run %q was cancelled (run.cancelled in log). Cannot resume a cancelled run; start a new run id.\n", runID)
			return ExitUsage
		}
	}
	// Step 4: refusal — node.failed already in the log (terminal-by-propagation).
	// Phase 2 has no try/catch (Phase 3 lights it up); a failed step halts the
	// run, and resuming would try to re-execute the failed step, which is not
	// the Phase-2 retry semantic. Refuse explicitly.
	for _, e := range events {
		if e.Type == engine.EventNodeFailed {
			fprintf(stderr, "awf resume: run %q terminated on a failed step (node.failed at path %q in log). Phase 2 does not resume past a failed step; Phase 3's try/catch will revisit this.\n", runID, e.Path)
			return ExitUsage
		}
	}

	// Step 5: load + validate + digest the workflow at wfPath. Failures here
	// are independent of the log — bad path / bad YAML / validator errors all
	// exit with the usual codes.
	ld, err := loader.Load(wfPath)
	if err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		digest, _ := ld.Workflow.ComputeDigest(ld.ComposeFiles)
		printTextResult(stderr, wfPath, digest, diags)
		return ExitInvalid
	}
	currentDigest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles)
	if err != nil {
		fprintf(stderr, "awf resume: compute digest: %v\n", err)
		return ExitUsage
	}

	// Step 6: refusal — digest mismatch (spec §8 hard error). The folded
	// rs.WorkflowDigest came from the log's run.started event; a workflow file
	// that hashes differently is a forbidden definition change.
	if rs.WorkflowDigest != currentDigest {
		fprintf(stderr, "awf resume: workflow digest mismatch — run %q was started with digest %q, file %q now hashes to %q. Spec §8 forbids resuming against a changed definition.\n",
			runID, rs.WorkflowDigest, wfPath, currentDigest)
		return ExitUsage
	}

	// Step 7: signal handling — same model as cli/run.go.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Slice 3.5: per-run broker for the engine + clear stale pause.json /
	// cancel.json so the first poll doesn't immediately re-pause/cancel.
	controlDir := awfsignal.ControlDir(*stateDir, runID)
	broker := awfsignal.NewBroker(controlDir, r.BrokerOptions...)
	if err := broker.ClearPauseCancel(); err != nil {
		fprintf(stderr, "awf resume: clear pause/cancel files: %v\n", err)
		return ExitUsage
	}

	// Step 8: Create container handles. SAME pattern as cli/run.go — handles
	// are CLI-owned (slice 2.5 Design question 3); resume rebuilds them from
	// the workflow's containers map. Phase 2 fake: factory() per Create.
	// Phase 4 Docker will honor the image / compose recipe (spec §8
	// "containers are reconstructed from their image/compose recipe on every
	// (re)creation, including resume").
	handles := make(map[string]container.Handle, len(ld.Workflow.Containers))
	skipTeardown := false
	defer func() {
		if skipTeardown {
			return
		}
		teardownCtx, cancel := context.WithTimeout(context.Background(), teardownGrace)
		defer cancel()
		for _, h := range handles {
			_ = r.Backend.Destroy(teardownCtx, h)
		}
	}()
	for name := range ld.Workflow.Containers {
		h, err := r.Backend.Create(ctx, engine.ContainerSpecFor(ld.Workflow, ld.ComposeFiles, name))
		if err != nil {
			fprintf(stderr, "awf resume: create container %q: %v\n", name, err)
			return ExitUsage
		}
		handles[name] = h
	}

	// Step 9: append run.resumed{epoch: rs.Epoch+1}. Slice 2.6 Design
	// question 6: the new epoch lives in the EVENT PAYLOAD (the resume
	// counter), distinct from FileLog's per-event Epoch field (which got
	// bumped by OpenLog already).
	newEpoch := rs.Epoch + 1
	resumedData, err := json.Marshal(engine.RunResumedData{Epoch: newEpoch})
	if err != nil {
		fprintf(stderr, "awf resume: marshal run.resumed: %v\n", err)
		return ExitUsage
	}
	if err := log.Append(state.Event{Type: engine.EventRunResumed, Data: resumedData}); err != nil {
		fprintf(stderr, "awf resume: append run.resumed: %v\n", err)
		return ExitUsage
	}
	if err := log.Sync(); err != nil {
		fprintf(stderr, "awf resume: sync log after run.resumed: %v\n", err)
		return ExitUsage
	}
	rs.Epoch = newEpoch

	// Step 10: dispatch engine.Run + write run.finished + map outcome → exit
	// code. See cli/execute.go: the closing sequence is shared with `awf run`.
	// The interpreter's resume-checks (slice 2.5: runstate.Completed /
	// Branches / LoopIters) skip already-committed nodes — same code path on
	// first run and resume (CLAUDE.md invariant).
	return r.runAndFinish(ctx, ld, rs, handles, log, blobs, stdout, stderr, runID, "awf resume", " (resumed)", broker, &skipTeardown)
}
