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
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// printRunUsage writes the run-subcommand usage line.
func printRunUsage(w io.Writer) {
	fprintln(w, "usage: awf run [--input <json>] [--run-id <id>] [--state-dir <dir>] <path>")
	fprintln(w, "")
	fprintln(w, "  --input <json>     run-input as a JSON object (validated against workflow.input schema if declared)")
	fprintln(w, "  --run-id <id>      override the minted run id (testing aid)")
	fprintln(w, "  --state-dir <dir>  base directory for .awf/runs and .awf/blobs (default: ./.awf)")
}

// teardownGrace is how long Backend.Destroy gets after the run's ctx has been
// cancelled (Ctrl-C / SIGTERM). The user's signal cancels in-flight work via
// ctx, but the deferred Destroy needs a non-cancelled ctx so containers
// actually come down. 30s is generous for Phase 2 fake (instant) and Phase 4
// Docker (`docker stop --time=10s` + cleanup is typically <30s).
const teardownGrace = 30 * time.Second

// cliRun implements `awf run`. See plan §G + slice 2.5 self-critique round 2
// for the operation ordering rationale (OpenLogExclusive runs LAST among
// could-fail setup steps to minimize the orphan-log window).
func (r *Runner) cliRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	inputJSON := flags.String("input", "", "run-input JSON")
	runID := flags.String("run-id", "", "override the run id")
	stateDir := flags.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printRunUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf run: %v\n", err)
		printRunUsage(stderr)
		return ExitUsage
	}
	if flags.NArg() != 1 {
		printRunUsage(stderr)
		return ExitUsage
	}
	path := flags.Arg(0)

	// Step 1: load + validate + digest.
	ld, err := loader.Load(path)
	if err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		digest, _ := ld.Workflow.ComputeDigest(ld.ComposeFiles)
		printTextResult(stderr, path, digest, diags)
		return ExitInvalid
	}
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles)
	if err != nil {
		fprintf(stderr, "awf run: compute digest: %v\n", err)
		return ExitUsage
	}

	// Step 2: mint run.id.
	id := *runID
	if id == "" {
		id = r.IDGen.NewRunID()
	}

	// Step 3: parse + schema-validate --input BEFORE any state is created
	// on disk. Pre-flight rejection here avoids orphan log files on bad input.
	var inputMap map[string]any
	if *inputJSON != "" {
		if ld.Workflow.Input == nil {
			// --input is only meaningful when workflow declares an input schema.
			// Quiet acceptance is a confused-deputy smell for a security tool.
			fprintf(stderr, "awf run: --input provided but workflow declares no input schema. Drop --input or add an `input:` schema to the workflow.\n")
			return ExitUsage
		}
		m, err := engine.ValidateAgainstSchema([]byte(*inputJSON), ld.Workflow.Input)
		if err != nil {
			fprintf(stderr, "awf run: --input failed validation: %v\n", err)
			return ExitUsage
		}
		inputMap = m
	}

	// Step 4: wire signal handling. signal.NotifyContext (Go 1.16+) is canonical.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 5: Create container handles. Defer Destroy with a SEPARATE
	// non-cancelled ctx so teardown survives signal-induced cancellation.
	handles := make(map[string]container.Handle, len(ld.Workflow.Containers))
	// Register the teardown defer BEFORE Create so a mid-Create failure still
	// cleans up the handles that were already created. The closure reads
	// `handles` at defer-time, so it sees whatever was successfully created.
	// Latent Phase 4 hazard (Phase 2 fake's Create can't fail; Phase 4 Docker
	// can): without this ordering, a 2-container workflow with the second
	// Create failing would leak the first container.
	defer func() {
		teardownCtx, cancel := context.WithTimeout(context.Background(), teardownGrace)
		defer cancel()
		for _, h := range handles {
			_ = r.Backend.Destroy(teardownCtx, h)
		}
	}()
	for name := range ld.Workflow.Containers {
		h, err := r.Backend.Create(ctx, container.ContainerSpec{Name: name})
		if err != nil {
			fprintf(stderr, "awf run: create container %q: %v\n", name, err)
			return ExitUsage
		}
		handles[name] = h
	}

	// Step 6: open blobs (shared CAS dir; idempotent across runs).
	blobsDir := filepath.Join(*stateDir, "blobs")
	blobs, err := state.OpenBlobs(blobsDir)
	if err != nil {
		fprintf(stderr, "awf run: open blobs %q: %v\n", blobsDir, err)
		return ExitUsage
	}

	// Step 7: put input into Blobs (after validation, before log creation).
	var inputRef string
	if *inputJSON != "" {
		ref, err := blobs.Put([]byte(*inputJSON))
		if err != nil {
			fprintf(stderr, "awf run: put input: %v\n", err)
			return ExitUsage
		}
		inputRef = ref
	}

	// Step 8: OpenLogExclusive atomically claims the run.id. Cleanup-on-error
	// defer removes the empty log if run.started never lands.
	runDir := filepath.Join(*stateDir, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fprintf(stderr, "awf run: create run dir %q: %v\n", runDir, err)
		return ExitUsage
	}
	logPath := filepath.Join(runDir, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			fprintf(stderr, "awf run: run id %q already exists at %q — use `awf resume` to continue an interrupted run, or pick a different --run-id\n", id, logPath)
		} else {
			fprintf(stderr, "awf run: open log %q: %v\n", logPath, err)
		}
		return ExitUsage
	}
	runStartedAppended := false
	defer func() {
		_ = log.Close()
		if !runStartedAppended {
			_ = os.Remove(logPath)
		}
	}()

	// Step 9: append run.started + fsync.
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:          id,
		WorkflowDigest: digest,
		InputRef:       inputRef,
	})
	if err != nil {
		fprintf(stderr, "awf run: marshal run.started: %v\n", err)
		return ExitUsage
	}
	if err := log.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: runStartedData,
	}); err != nil {
		fprintf(stderr, "awf run: append run.started: %v\n", err)
		return ExitUsage
	}
	if err := log.Sync(); err != nil {
		fprintf(stderr, "awf run: sync log after run.started: %v\n", err)
		return ExitUsage
	}
	runStartedAppended = true

	// Step 10: build RunState; dispatch engine.Run + write run.finished +
	// map outcome → exit code. See cli/execute.go: the closing sequence is
	// shared with `awf resume`.
	rs := engine.NewRunState(id, digest, inputMap)
	return r.runAndFinish(ctx, ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "")
}
