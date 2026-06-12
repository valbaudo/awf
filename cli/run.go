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
	fprintln(w, "usage: awf run [--input <json>] [--run-id <id>] [--state-dir <dir>] [--backend <auto|native|docker|fake>] [--agent-env <CSV>] <path>")
	fprintln(w, "")
	fprintln(w, "  --input <json>     run-input as a JSON object (validated against workflow.input schema if declared)")
	fprintln(w, "  --run-id <id>      override the minted run id (testing aid)")
	fprintln(w, "  --state-dir <dir>  base directory for .awf/runs and .awf/blobs (default: ./.awf)")
	fprintln(w, "  --backend <kind>   container backend: \"auto\", \"native\", \"docker\", or \"fake\" (default: auto)")
	fprintln(w, "  --agent-env <CSV>  env-var allowlist forwarded into agent CLIs (default: "+strings.Join(defaultAgentEnv, ",")+")")
}

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
	backendKind := flags.String("backend", backendAuto, "container backend: auto, native, docker, or fake")
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
	path := flags.Arg(0)

	// Step 1: load + validate + digest.
	ld, err := loader.Load(path)
	if err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		digest, _ := ld.ComputeDigest()
		printTextResult(stderr, path, digest, diags)
		return ExitInvalid
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		fprintf(stderr, "awf run: compute digest: %v\n", err)
		return ExitUsage
	}

	concreteBackendKind, err := selectRunBackendForLoadedDefinition(*backendKind, ld)
	if err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	autoSelectedNative := *backendKind == backendAuto && concreteBackendKind == engine.BackendNative

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
	backend, cleanup, err := r.resolveBackend(ctx, concreteBackendKind, id, workdirRoot, blobs)
	if err != nil {
		fprintf(stderr, "awf run: construct backend %q: %v\n", concreteBackendKind, err)
		return ExitUsage
	}
	defer cleanup()

	if err := checkWorkflowBackendCapabilities(ld, concreteBackendKind, backend); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	liveRoot, err := openLiveHomeRoot(*stateDir)
	if err != nil {
		fprintf(stderr, "awf run: open live home: %v\n", err)
		return ExitUsage
	}

	// Slice 5.3: if Resolver isn't test-injected, build the production
	// *agent.Registry from --agent-env + the resolved backend. Tests that
	// inject r.Resolver skip this step entirely.
	resolver := r.Resolver
	if resolver == nil {
		// The forwarded allowlist is the --agent-env flag (or its default) plus
		// the workflow's own top-level env: names (awf-workflow(5)). Names only —
		// values resolve from the host inside buildAgentRegistry.
		envNames := mergeLoadedWorkflowEnv(parseCSV(*agentEnv), ld)
		reg, err := buildAgentRegistryWithLiveRoot(envNames, backend, liveRoot)
		if err != nil {
			fprintf(stderr, "awf run: build agent registry: %v\n", err)
			return ExitUsage
		}
		// C3: register one DerivedAdapter per declared agents: role on top of the
		// base adapters, so `uses: <role>` resolves and the role is pinned as a
		// first-class runtime. Same fail-loud path as an unknown adapter.
		if err := registerRolesForLoadedDefinition(reg, ld); err != nil {
			fprintf(stderr, "awf run: register agent roles: %v\n", err)
			return ExitUsage
		}
		resolver = reg
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
		teardownCtx, cancel := context.WithTimeout(context.Background(), container.TeardownGrace)
		defer cancel()
		for _, h := range handles {
			_ = backend.Destroy(teardownCtx, h)
		}
	}()
	// A map's runtime-resolved image: target (P6a) is NOT pre-provisioned here:
	// it carries no declared image (AWF1025), and its per-element image is
	// learned + Created at dispatch time (engine/map.go). Pre-Creating it would
	// fail (empty image). Skip it; the map handler owns its lifecycle.
	mapImageTargets := ir.MapImageTargets(ld.Workflow)
	for name := range ld.Workflow.Containers {
		if mapImageTargets[name] {
			continue
		}
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
	resolvedRuntimes, err := resolveRuntimes(ctx, agentRefs, resolverOrEmpty(resolver), handles)
	if err != nil {
		fprintf(stderr, "awf run: resolve agent runtimes: %v\n", err)
		return ExitUsage
	}

	// Part D: fail fast if any `continues:` step's adapter is not Threaded
	// (mirrors the Containerless guard inside resolveRuntimes). Runs before
	// the log is opened, so a rejected run leaves no state on disk.
	if err := checkThreadedAdaptersForLoadedDefinition(ld, resolverOrEmpty(resolver)); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	if err := checkPersistentSessionGateEvaluateForLoadedDefinition(ld, resolverOrEmpty(resolver)); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
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

	assetSnapshots, err := engine.StoreRunStartedAssetsForLoadedDefinition(blobs, ld)
	if err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
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

	// Step 10: append run.started + fsync. Backend field carries the selected
	// concrete backend kind so resume can pick the same backend without a flag.
	if autoSelectedNative {
		fprintf(stderr, "awf run: backend auto selected native; this run cannot be resumed until native resume is supported. Use --backend docker for resumable runs.\n")
	}
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:           id,
		WorkflowDigest:  digest,
		WorkflowID:      ld.Workflow.ID,      // slice 6.1 — obs awf.workflow.id (standard §9)
		WorkflowVersion: ld.Workflow.Version, // slice 6.1 — obs awf.workflow.version
		InputRef:        inputRef,
		Backend:         concreteBackendKind,
		Assets:          assetSnapshots,
		LiveHome:        engineLiveHomePin(liveRoot.Pin),
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
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "", assetSnapshots, broker, liveRoot, &skipTeardown)
}
