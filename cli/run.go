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
	"strings"
	"syscall"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	awfsignal "github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// printRunUsage writes the run-subcommand usage line.
func printRunUsage(w io.Writer) {
	fprintln(w, "usage: awf run [--input <json>] [--run-id <id>] [--state-dir <dir>] [--backend <fake|docker|native>] [--agent-env <CSV>] <path>")
	fprintln(w, "")
	fprintln(w, "  --input <json>     run-input as a JSON object (validated against workflow.input schema if declared)")
	fprintln(w, "  --run-id <id>      override the minted run id (testing aid)")
	fprintln(w, "  --state-dir <dir>  base directory for .awf/runs and .awf/blobs (default: ./.awf)")
	fprintln(w, "  --backend <kind>   container backend: \"fake\", \"docker\", or \"native\" (default: native)")
	fprintln(w, "  --agent-env <CSV>  env-var allowlist forwarded into agent CLIs (default: "+strings.Join(defaultAgentEnv, ",")+")")
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
	backendKind := flags.String("backend", engine.BackendNative, "container backend: fake, docker, or native")
	agentEnv := flags.String("agent-env", strings.Join(defaultAgentEnv, ","),
		"CSV allowlist of env-var names forwarded into each agent CLI invocation")
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
	switch *backendKind {
	case engine.BackendFake, engine.BackendDocker, engine.BackendNative:
		// ok
	default:
		fprintf(stderr, "awf run: invalid --backend value %q; want %q, %q, or %q\n", *backendKind, engine.BackendFake, engine.BackendDocker, engine.BackendNative)
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

	// Slice 4.7 decision 1 + H8 fix: if --backend native is selected but
	// any container declares compose: mode, fail-fast at the CLI layer
	// (defense-in-depth: Backend.Create also rejects). Friendlier than
	// a mid-run error after partial setup.
	if *backendKind == engine.BackendNative {
		for name, c := range ld.Workflow.Containers {
			if c.Compose != "" {
				fprintf(stderr, "awf run: --backend native cannot run workflows with compose-mode containers (container %q declares compose: %q). Use --backend docker.\n", name, c.Compose)
				return ExitUsage
			}
		}
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

	// Step 4: wire signal handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 5: open blobs (moved ahead of Backend construction + Create-handles
	// in slice 4.5 because the Docker newBackend needs blobs to construct).
	blobsDir := filepath.Join(*stateDir, "blobs")
	blobs, err := state.OpenBlobs(blobsDir)
	if err != nil {
		fprintf(stderr, "awf run: open blobs %q: %v\n", blobsDir, err)
		return ExitUsage
	}

	// Step 6 (slice 4.5): determine the Backend for this invocation via
	// resolveBackend (cli/backend.go) — uses test-injected r.Backend when
	// set, else constructs via newBackend using the --backend flag value.
	// The result is held in a LOCAL variable (NEVER assigned to r.Backend)
	// so sequential runner.Run(...) calls don't leak a constructed Backend.
	workdirRoot := filepath.Join(*stateDir, "work")
	backend, cleanup, err := r.resolveBackend(ctx, *backendKind, id, workdirRoot, blobs)
	if err != nil {
		fprintf(stderr, "awf run: construct backend %q: %v\n", *backendKind, err)
		return ExitUsage
	}
	defer cleanup()

	// Slice 7.1 capability guard: a snapshot:workspace container on a backend
	// that can't snapshot (native: no CoW) must fail-fast here — same fail-fast
	// posture as the compose+native rejection above — rather than mid-run at
	// the dispatcher's Snapshot call.
	if err := checkSnapshotCapability(ld.Workflow, backend); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	// Slice 5.3: if Resolver isn't test-injected, build the production
	// *agent.Registry from --agent-env + the resolved backend. Tests that
	// inject r.Resolver skip this step entirely.
	if r.Resolver == nil {
		// The forwarded allowlist is the --agent-env flag (or its default) plus
		// the workflow's own top-level env: names (awf-workflow(5)). Names only —
		// values resolve from the host inside buildAgentRegistry.
		envNames := mergeWorkflowEnv(parseCSV(*agentEnv), ld.Workflow.Env)
		reg, err := buildAgentRegistry(envNames, backend)
		if err != nil {
			fprintf(stderr, "awf run: build agent registry: %v\n", err)
			return ExitUsage
		}
		r.Resolver = reg
	}

	// Step 7: Create container handles. Defer Destroy with a SEPARATE
	// non-cancelled ctx so teardown survives signal-induced cancellation.
	// Register the teardown defer BEFORE Create so a mid-Create failure still
	// cleans up the handles that were already created. The closure reads
	// `handles` at defer-time, so it sees whatever was successfully created.
	// Latent Phase 4 hazard (Phase 2 fake's Create can't fail; Phase 4 Docker
	// can): without this ordering, a 2-container workflow with the second
	// Create failing would leak the first container.
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
	for name := range ld.Workflow.Containers {
		h, err := backend.Create(ctx, engine.ContainerSpecFor(ld.Workflow, ld.ComposeFiles, name))
		if err != nil {
			fprintf(stderr, "awf run: create container %q: %v\n", name, err)
			return ExitUsage
		}
		handles[name] = h
	}

	// Phase 5 slice 5.1: walk for agent refs, resolve versions per (uses,
	// container) pair, persist into RunStartedData.Runtimes below. Empty
	// workflow (no `uses:` steps — every Phase 2-4 fixture) → resolvedRuntimes
	// is nil; omitempty on the field means run.started JSON has no "runtimes"
	// key, byte-equal to pre-Phase-5 logs.
	agentRefs := walkAgentRefs(ld.Workflow)
	resolvedRuntimes, err := resolveRuntimes(ctx, agentRefs, r.resolverOrEmpty(), handles)
	if err != nil {
		fprintf(stderr, "awf run: resolve agent runtimes: %v\n", err)
		return ExitUsage
	}

	// Step 8: put input into Blobs (after validation, before log creation).
	var inputRef string
	if *inputJSON != "" {
		ref, err := blobs.Put([]byte(*inputJSON))
		if err != nil {
			fprintf(stderr, "awf run: put input: %v\n", err)
			return ExitUsage
		}
		inputRef = ref
	}

	// Step 9: OpenLogExclusive atomically claims the run.id.
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

	// Slice 6.2: hold a sidecar flock for the run's lifetime so `awf ls` can
	// tell this live run from a crashed one. The kernel releases it on any
	// death; Release fires on clean exit. The lock is liveness metadata, not
	// durable state (it never touches the log or blobs).
	lock, lockErr := acquireRunLock(runDir)
	if lockErr != nil {
		if errors.Is(lockErr, ErrRunLockHeld) {
			fprintf(stderr, "awf run: run id %q is already active in another process\n", id)
		} else {
			fprintf(stderr, "awf run: acquire run lock: %v\n", lockErr)
		}
		return ExitUsage
	}
	defer lock.Release()

	// Step 10: append run.started + fsync. Backend field carries the slice-
	// 4.5 --backend kind so resume can pick the same backend without a flag.
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:           id,
		WorkflowDigest:  digest,
		WorkflowID:      ld.Workflow.ID,      // slice 6.1 — obs awf.workflow.id (standard §9)
		WorkflowVersion: ld.Workflow.Version, // slice 6.1 — obs awf.workflow.version
		InputRef:        inputRef,
		Backend:         *backendKind,
		Runtimes:        resolvedRuntimes, // Phase 5 slice 5.1
	})
	if err != nil {
		fprintf(stderr, "awf run: marshal run.started: %v\n", err)
		return ExitUsage
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		fprintf(stderr, "awf run: append run.started: %v\n", err)
		return ExitUsage
	}
	if err := log.Sync(); err != nil {
		fprintf(stderr, "awf run: sync log after run.started: %v\n", err)
		return ExitUsage
	}
	runStartedAppended = true

	// Step 11: build RunState; dispatch engine.Run + write run.finished +
	// map outcome → exit code. backend (local) is passed in — runAndFinish
	// does NOT read r.Backend.
	rs := engine.NewRunState(id, digest, inputMap)
	// Slice 3.5: per-run broker. The control directory is created lazily on
	// first WriteSignal / WritePause / WriteCancel from another process.
	// r.BrokerOptions defaults to empty (100ms poll) in production; tests
	// inject signal.WithPollInterval(time.Millisecond) for fast runs.
	broker := awfsignal.NewBroker(awfsignal.ControlDir(*stateDir, id), r.BrokerOptions...)
	return r.runAndFinish(ctx, backend, ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "", broker, &skipTeardown)
}
