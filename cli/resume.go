package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

// ErrRuntimeDrift is the hard-error returned when a resumed run's agent
// runtime version no longer matches what was persisted in run.started.
// Mirrors the spec §8 pinning invariant (the existing definition-digest
// hard-error class); resume cannot adapt to a changed binary.
//
// Phase 5 slice 5.1: scoped per (Ref, Container) pair, since different
// containers may have different `claude` binaries on PATH (decision 5).
type ErrRuntimeDrift struct {
	Ref       string
	Container string
	Recorded  string
	Current   string
}

func (e *ErrRuntimeDrift) Error() string {
	return fmt.Sprintf(
		"cli: agent runtime drift for %q in container %q: recorded %q, now %q — cannot resume (spec §8 pinning is a hard error)",
		e.Ref, e.Container, e.Recorded, e.Current,
	)
}

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
//  4. Refuse on terminal events: run.finished / run.cancelled / node.failed.
//  5. (slice 4.5) Wire signal handling EARLY so newBackend gets a real ctx.
//  6. (slice 4.5) Read the Backend kind from the folded log's run.started;
//     construct the Backend via newBackend if r.Backend is nil. Hold in a
//     LOCAL variable; never assign to r.Backend.
//  7. Load + validate + digest the workflow file.
//  8. Refuse on digest mismatch (spec §8 hard error).
//  9. Broker + ClearPauseCancel.
//  10. Create container handles.
//  11. Append run.resumed{epoch: rs.Epoch+1} + Sync.
//  12. runAndFinish (shared with `awf run`).
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

	// Slice 6.2: hold the run-lifetime flock for this resume epoch so `awf ls`
	// sees the run as running again. Refuse if another process already drives
	// it (double-driving one run corrupts nothing durable but is never intended).
	runDir := filepath.Join(*stateDir, "runs", runID)
	lock, lockErr := acquireRunLock(runDir)
	if lockErr != nil {
		if errors.Is(lockErr, ErrRunLockHeld) {
			fprintf(stderr, "awf resume: run %q is already active in another process; refusing to resume a live run\n", runID)
		} else {
			fprintf(stderr, "awf resume: acquire run lock: %v\n", lockErr)
		}
		return ExitUsage
	}
	defer lock.Release()

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

	// Step 3: terminal-event refusals (precedence: run.finished →
	// run.cancelled → node.failed; each error message names the event).
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			fprintf(stderr, "awf resume: run %q already finished (run.finished event in log). Cannot resume a completed run.\n", runID)
			return ExitUsage
		}
	}
	// Slice 3.5: refusal — run.cancelled already in log (terminal).
	// Checked BEFORE node.failed: cancel-during-step writes both events; the
	// user wants to see "cancelled," not "failed step."
	for _, e := range events {
		if e.Type == engine.EventRunCancelled {
			fprintf(stderr, "awf resume: run %q was cancelled (run.cancelled in log). Cannot resume a cancelled run; start a new run id.\n", runID)
			return ExitUsage
		}
	}
	// Refusal — node.failed already in the log (terminal-by-propagation).
	// Phase 2 has no try/catch (Phase 3 lights it up); a failed step halts the
	// run, and resuming would try to re-execute the failed step, which is not
	// the Phase-2 retry semantic. Refuse explicitly.
	for _, e := range events {
		if e.Type == engine.EventNodeFailed {
			fprintf(stderr, "awf resume: run %q terminated on a failed step (node.failed at path %q in log). Phase 2 does not resume past a failed step; Phase 3's try/catch will revisit this.\n", runID, e.Path)
			return ExitUsage
		}
	}

	// Step 4 (slice 4.5): wire signal handling EARLY so newBackend gets a
	// real ctx. Moved ahead of backend construction; semantically unchanged
	// (ctx is still used by Create-handles + runAndFinish below).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 5 (slice 4.5): read the Backend kind from the log's run.started
	// and resolve the Backend (r.Backend if test-injected, else newBackend
	// via resolveBackend in cli/backend.go). Result is a LOCAL variable
	// (NEVER assigned to r.Backend). No --backend flag on resume (per
	// Phase 4 design § F): the originating `awf run` recorded the kind in
	// run.started, and resume MUST use the same one — picking it from the
	// log is the only way to satisfy that without a flag.
	kind, err := readBackendKindFromLog(events)
	if err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}
	workdirRoot := filepath.Join(*stateDir, "work")
	backend, cleanup, err := r.resolveBackend(ctx, kind, runID, workdirRoot, blobs)
	if err != nil {
		fprintf(stderr, "awf resume: construct backend %q: %v\n", kind, err)
		return ExitUsage
	}
	defer cleanup()

	// Step 6: load + validate + digest the workflow at wfPath. Failures here
	// are independent of the log — bad path / bad YAML / validator errors all
	// exit with the usual codes.
	ld, err := loader.Load(wfPath)
	if err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		digest, _ := ld.ComputeDigest()
		printTextResult(stderr, wfPath, digest, diags)
		return ExitInvalid
	}
	currentDigest, err := ld.ComputeDigest()
	if err != nil {
		fprintf(stderr, "awf resume: compute digest: %v\n", err)
		return ExitUsage
	}

	// Step 7: refusal — digest mismatch (spec §8 hard error). The folded
	// rs.WorkflowDigest came from the log's run.started event; a workflow file
	// that hashes differently is a forbidden definition change.
	if rs.WorkflowDigest != currentDigest {
		fprintf(stderr, "awf resume: workflow digest mismatch — run %q was started with digest %q, file %q now hashes to %q. Spec §8 forbids resuming against a changed definition.\n",
			runID, rs.WorkflowDigest, wfPath, currentDigest)
		return ExitUsage
	}
	started, err := engine.RunStartedDataFromEvents(events)
	if err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}
	recordedAssets := started.Assets

	// Slice 5.3: if Resolver isn't test-injected, build the production
	// *agent.Registry. `awf resume` does not accept --agent-env (per Phase 5
	// design § E — it re-reads env from the host with the standard default set),
	// but the workflow's own top-level env: names extend that allowlist, re-read
	// from the host on resume exactly as on run. Built AFTER the load+digest
	// checks above so ld.Workflow.Env is available (the digest pins those names,
	// so a changed env: declaration has already hard-errored at the mismatch
	// check; the host VALUES are not pinned and re-resolve here).
	if r.Resolver == nil {
		reg, err := buildAgentRegistry(mergeWorkflowEnv(defaultAgentEnv, ld.Workflow.Env), backend)
		if err != nil {
			fprintf(stderr, "awf resume: build agent registry: %v\n", err)
			return ExitUsage
		}
		// C3: re-register the agents: roles on resume so `uses: <role>` resolves
		// and the role's pinned runtime is re-resolved for the drift check. The
		// definition digest already pinned the role bindings (a changed agents:
		// has hard-errored at the digest mismatch above), so re-registration is
		// the same deterministic resolution as run-start.
		if err := registerRoles(reg, ld.Workflow); err != nil {
			fprintf(stderr, "awf resume: register agent roles: %v\n", err)
			return ExitUsage
		}
		r.Resolver = reg
	}

	// Step 8 (slice 3.5): per-run broker for the engine + clear stale
	// pause.json / cancel.json so the first poll doesn't immediately
	// re-pause/cancel.
	controlDir := awfsignal.ControlDir(*stateDir, runID)
	broker := awfsignal.NewBroker(controlDir, r.BrokerOptions...)
	if err := broker.ClearPauseCancel(); err != nil {
		fprintf(stderr, "awf resume: clear pause/cancel files: %v\n", err)
		return ExitUsage
	}

	// Step 9: Create container handles. SAME pattern as cli/run.go — handles
	// are CLI-owned (slice 2.5 Design question 3); resume rebuilds them from
	// the workflow's containers map. Phase 2 fake: factory() per Create.
	// Phase 4 Docker honors the image / compose recipe (spec §8 "containers
	// are reconstructed from their image/compose recipe on every (re)creation,
	// including resume").
	//
	if err := checkWorkflowBackendCapabilities(ld.Workflow, kind, backend); err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}

	handles := make(map[string]container.Handle, len(ld.Workflow.Containers))
	skipTeardown := false
	defer func() {
		if skipTeardown {
			return
		}
		teardownCtx, cancel := context.WithTimeout(context.Background(), teardownGrace)
		defer cancel()
		for _, h := range handles {
			_ = backend.Destroy(teardownCtx, h)
		}
	}()
	// Slice 7.1: a snapshot:workspace container with a folded ref is RESTORED
	// from its latest committed snapshot (resume folds the log; committed work
	// is replayed, not recomputed). Every other container — and a
	// snapshot:workspace one with NO ref (crashed before its first commit) —
	// takes the Create path: infra rebuilt from its image/compose recipe.
	// A map's runtime-resolved image: target (P6a) is NOT pre-provisioned here
	// (nor restored): it carries no declared image, and its per-element image is
	// learned + Created at dispatch time (engine/map.go). Skip it on resume too —
	// the map handler re-creates per-item handles for the uncommitted frontier.
	mapImageTargets := ir.MapImageTargets(ld.Workflow)
	for name, c := range ld.Workflow.Containers {
		if mapImageTargets[name] {
			continue
		}
		var h container.Handle
		var err error
		if c.Snapshot == "workspace" && rs.SnapshotRefs[name] != "" {
			h, err = backend.Restore(ctx, container.SnapshotRef(rs.SnapshotRefs[name]), name)
		} else {
			h, err = backend.Create(ctx, engine.ContainerSpecFor(ld.Workflow, ld.ComposeFiles, name))
		}
		if err != nil {
			fprintf(stderr, "awf resume: create/restore container %q: %v\n", name, err)
			return ExitUsage
		}
		handles[name] = h
	}

	// Step 9.5 (slice 5.1): agent runtime version pinning check (parallels
	// the definition-digest hard-error class from Step 7). Re-walk the
	// workflow's `uses:` refs, re-resolve via the live registry + handles
	// from Step 9, hard-error on any mismatch with the Runtimes recorded in
	// run.started. Per spec §8: pinning is a hard error on drift.
	recordedRuntimes, err := readRuntimesFromLog(events)
	if err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}
	agentRefs := walkAgentRefs(ld.Workflow)
	currentRuntimes, err := resolveRuntimes(ctx, agentRefs, r.resolverOrEmpty(), handles)
	if err != nil {
		fprintf(stderr, "awf resume: resolve agent runtimes: %v\n", err)
		return ExitUsage
	}
	if err := checkRuntimesDrift(recordedRuntimes, currentRuntimes); err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}

	// Part D: same Threaded guard as run-start (continues: against a
	// non-Threaded adapter). Run before appending run.resumed so a rejected
	// resume is a no-op on the log.
	if err := checkThreadedAdapters(ld.Workflow, r.resolverOrEmpty()); err != nil {
		fprintf(stderr, "awf resume: %v\n", err)
		return ExitUsage
	}

	// Step 10: append run.resumed{epoch: rs.Epoch+1}. Slice 2.6 Design
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

	// Step 11: dispatch engine.Run + write run.finished + map outcome → exit
	// code. See cli/execute.go: the closing sequence is shared with `awf run`.
	// The interpreter's resume-checks (slice 2.5: runstate.Completed /
	// Branches / LoopIters) skip already-committed nodes — same code path on
	// first run and resume (CLAUDE.md invariant). backend (local) is passed
	// in — runAndFinish does NOT read r.Backend.
	return r.runAndFinish(ctx, backend, ld, rs, handles, log, blobs, stdout, stderr, runID, "awf resume", " (resumed)", recordedAssets, broker, &skipTeardown)
}
