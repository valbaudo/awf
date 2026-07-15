package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	awfsignal "github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// resolveWorkflowRunEnv resolves the workflow's own declared env: allowlist
// (module-recursive; awf-workflow(5)) against the host into a name→value map
// for engine.RunOptions.RunEnv (F15: env: also reaches run: steps, not just
// agents). Reuses mergeLoadedWorkflowEnv with a nil base — the --agent-env
// allowlist is agent-only and must NOT leak into this channel — so the
// returned names are exactly (and only) what the workflow itself declares.
// Each name is resolved via os.LookupEnv here, at the CLI layer: the engine
// itself never reads host env (determinism invariant). A name absent from the
// host is silently omitted, mirroring buildAgentRegistryWithLiveRoot's
// resolution loop. nil when the workflow declares no env: names — preserves
// pre-F15 behavior (code steps get an empty Env).
func resolveWorkflowRunEnv(ld *ir.LoadedDefinition) map[string]string {
	names := mergeLoadedWorkflowEnv(nil, ld)
	if len(names) == 0 {
		return nil
	}
	env := make(map[string]string, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	return env
}

// nativeSandboxModeOf returns backend's resolved sandbox mode when it is a
// concrete *native.Backend, or "" for every other backend kind (docker, fake,
// including a test-injected fake standing in for a "selected native"
// backend-selection test that never constructs a real native.Backend). ""
// only ever means "not native" — a real native.Backend's SandboxMode() is
// always non-empty ("none" at minimum). native.Backend.SandboxMode is a pure
// getter (no exec), so calling it here does not drive a real sandboxed
// execution.
func nativeSandboxModeOf(backend container.Backend) string {
	if nb, ok := backend.(*native.Backend); ok {
		return nb.SandboxMode()
	}
	return ""
}

// printNativeSandboxMode writes the resolved native sandbox mode to stderr
// (F30) when backend is a concrete *native.Backend. A no-op for every other
// backend kind (nativeSandboxModeOf returns "").
func printNativeSandboxMode(backend container.Backend, stderr io.Writer) {
	if mode := nativeSandboxModeOf(backend); mode != "" {
		fprintf(stderr, "awf run: native sandbox: %s\n", mode)
	}
}

// printRunUsage writes the run-subcommand usage line.
func printRunUsage(w io.Writer) {
	fprintln(w, "usage: awf run [--input <json>|--input-file <path>] [--input-files <name=path>]... [--run-id <id>] [--state-dir <dir>] [--backend <auto|native|docker|fake>] [--agent-env <CSV>] <path>")
	fprintln(w, "")
	fprintln(w, "  --input <json>        run-input as a JSON object (validated against workflow.input schema if declared)")
	fprintln(w, "  --input-file <path>   run-input JSON read from a file, or '-' for stdin (mutually exclusive with --input)")
	fprintln(w, "  --input-files <e>     workflow input file as name=path; repeatable (legacy: a single comma-separated value)")
	fprintln(w, "  --run-id <id>         override the minted run id (testing aid)")
	fprintln(w, "  --state-dir <dir>     base directory for .awf/runs and .awf/blobs (default: ./.awf)")
	fprintln(w, "  --backend <kind>      container backend: \"auto\", \"native\", \"docker\", or \"fake\" (default: auto)")
	fprintln(w, "  --agent-env <CSV>     env-var allowlist forwarded into agent CLIs (default: "+strings.Join(defaultAgentEnv, ",")+")")
}

// cliRun implements `awf run`. See plan §G + slice 2.5 self-critique round 2
// for the operation ordering rationale (OpenLogExclusive runs LAST among
// could-fail setup steps to minimize the orphan-log window).
func (r *Runner) cliRun(args []string, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("run", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	inputJSON := flags.String("input", "", "run-input JSON")
	inputFile := flags.String("input-file", "", "run-input JSON from a file, or '-' for stdin (mutually exclusive with --input)")
	inputFiles := flags.StringArray("input-files", nil, "top-level workflow input file as name=path; repeatable (legacy: a single comma-separated value)")
	runID := flags.String("run-id", "", "override the run id")
	stateDir := flags.String("state-dir", defaultStateDir(), "base directory for runs/ and blobs/")
	backendKind := flags.String("backend", backendAuto, "container backend: auto, native, docker, or fake")
	agentEnv := flags.String("agent-env", strings.Join(defaultAgentEnv, ","),
		"CSV allowlist of env-var names forwarded into each agent CLI invocation")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
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

	// F4b (AWF1065): a bare `run:` step's F4a host-workspace handle carries no
	// image, so it cannot run under docker. Checked HERE — right after backend
	// resolution, before minting run.id / touching disk — so it also catches a
	// MIXED workflow where auto-resolution (above) picked docker because of an
	// unrelated image-backed container, not just an explicit --backend docker.
	if err := checkContainerlessRunCapability(ld, concreteBackendKind); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	// Step 2: mint run.id.
	id := *runID
	if id == "" {
		id = r.IDGen.NewRunID()
	}

	// Step 3: resolve + schema-validate run input BEFORE any state is created on
	// disk (pre-flight rejection avoids orphan log files on bad input). Input is
	// supplied inline via --input or from a file/stdin via --input-file (mutually
	// exclusive); either way the resolved bytes are validated against
	// workflow.input_schema and content-addressed identically below (Step 8).
	inputBytes, haveInput, inErr := resolveRunInput(*inputJSON, *inputFile, r.stdin())
	if inErr != nil {
		fprintf(stderr, "awf run: %v\n", inErr)
		return ExitUsage
	}
	var inputMap map[string]any
	if haveInput {
		if ld.Workflow.InputSchema == nil {
			// Run input is only meaningful when the workflow declares an input
			// schema. Quiet acceptance is a confused-deputy smell for a security tool.
			fprintf(stderr, "awf run: run input provided but workflow declares no input schema. Drop --input/--input-file or add an `input_schema:` schema to the workflow.\n")
			return ExitUsage
		}
		m, err := engine.ValidateAgainstSchema(inputBytes, ld.Workflow.InputSchema)
		if err != nil {
			fprintf(stderr, "awf run: run input failed validation: %v\n", err)
			return ExitUsage
		}
		inputMap = m
	}

	// Step 3b: parse + validate --input-files BEFORE any state is created on
	// disk (same orphan-log-avoidance rationale as --input). The bytes are
	// content-addressed later (Step 8b), once blobs is open; here we only
	// reject bad supply: malformed entries, undeclared names, unsupplied
	// declared names, and missing/unreadable paths. The required contract
	// mirrors the call-step input_files contract (ir.validateCallInputFiles):
	// every DECLARED name must be supplied and every SUPPLIED name must be
	// declared (a top-level run is the call-step's run-start equivalent).
	inputFilePaths, err := parseInputFilesCSV(*inputFiles)
	if err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	if err := validateSuppliedInputFiles(inputFilePaths, ld.Workflow.InputFiles); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	canonicalStateDir, err := accessStateDir(*stateDir, stateWriteCreate, r.stateIdentity())
	if err != nil {
		fprintf(stderr, "awf run: %s\n", formatStateError("access state directory", *stateDir, *stateDir, err, r.stateIdentity()))
		return ExitInfra
	}
	*stateDir = canonicalStateDir

	// Step 4: wire signal handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 5: open blobs (moved ahead of Backend construction + Create-handles
	// in slice 4.5 because the Docker newBackend needs blobs to construct).
	blobsDir := filepath.Join(*stateDir, "blobs")
	blobs, err := state.OpenBlobs(blobsDir)
	if err != nil {
		return reportStateFailure(stderr, "awf run", "open blob store", *stateDir, blobsDir, err, r.stateIdentity(), stateFailureInfra)
	}

	// Step 6 (slice 4.5): determine the Backend for this invocation via
	// resolveBackend (cli/backend.go) — uses test-injected r.Backend when
	// set, else constructs via newBackend using the --backend flag value.
	// The result is held in a LOCAL variable (NEVER assigned to r.Backend)
	// so sequential runner.Run(...) calls don't leak a constructed Backend.
	workdirRoot := filepath.Join(*stateDir, "work", id)
	backend, cleanup, err := r.resolveBackend(ctx, concreteBackendKind, id, workdirRoot, blobs, stderr)
	if err != nil {
		if concreteBackendKind == engine.BackendNative {
			return reportStateFailure(stderr, "awf run", "prepare native work directory", *stateDir, workdirRoot, err, r.stateIdentity(), stateFailureInfra)
		}
		fprintf(stderr, "awf run: construct backend %q: %v\n", concreteBackendKind, err)
		return ExitInfra
	}
	defer cleanup()

	// F30: surface the resolved native sandbox mode at run start. No-op for
	// non-native backends (the type assertion fails for docker/fake, including
	// test-injected fakes used to drive native backend-selection logic without
	// an actual native.Backend).
	printNativeSandboxMode(backend, stderr)

	if err := checkWorkflowBackendCapabilities(ld, concreteBackendKind, backend); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	liveRoot, err := openLiveHomeRoot(*stateDir)
	if err != nil {
		livePath := filepath.Join(*stateDir, "live")
		if override := liveHomeEnv()["AWF_LIVE_HOME"]; override != "" {
			livePath = override
		}
		return reportStateFailure(stderr, "awf run", "open live home", *stateDir, livePath, err, r.stateIdentity(), stateFailureInfra)
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
			if concreteBackendKind == engine.BackendNative {
				return reportStateFailure(stderr, "awf run", "create native work directory", *stateDir, workdirRoot, err, r.stateIdentity(), stateFailureInfra)
			}
			fprintf(stderr, "awf run: create container %q: %v\n", name, err)
			return ExitInfra
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
	// Advisory credential-presence preflight: warns (never fails) when none of
	// an adapter's credential env vars is set — surfaces a likely Launch failure
	// before any container boots, without reversing the "auth fails at Launch"
	// contract.
	if err := checkCredentialPresenceForLoadedDefinition(ld, resolverOrEmpty(resolver), stderr); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	// Advisory idle-vs-liveness preflight: warns (never fails) when a step sets
	// timeout.idle on a blind adapter (SurfacesLiveness=None), where idle can only
	// behave as a wall-clock deadline. Surfaces the footgun before any container boots.
	if err := checkIdleLivenessForLoadedDefinition(ld, resolverOrEmpty(resolver), stderr); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	// Run-start with:-config guard: validate each agent step's adapter config
	// before the log opens, so config errors fail pre-spend (ExitUsage) rather
	// than mid-run (permanent_failure). Delegates to each adapter's
	// ValidateConfig through the Adapter seam (with: stays opaque to the core).
	if err := checkWithConfigForLoadedDefinition(ld, resolverOrEmpty(resolver), stderr); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}
	// Run-start containerless-input_files guard (F31b): a containerless agent
	// step's input_files are resolved and handed to the adapter as inline
	// message parts; an adapter that does not inline them would silently drop
	// the files at Launch. Fail fast, pre-spend, instead of that silent drop.
	if err := checkInputFilesForLoadedDefinition(ld, resolverOrEmpty(resolver), stderr); err != nil {
		fprintf(stderr, "awf run: %v\n", err)
		return ExitUsage
	}

	// Step 8: put input into Blobs (after validation, before log creation).
	var inputRef string
	if haveInput {
		ref, err := blobs.Put(inputBytes)
		if err != nil {
			return reportStateFailure(stderr, "awf run", "store run input", *stateDir, blobsDir, err, r.stateIdentity(), stateFailureInfra)
		}
		inputRef = ref
	}

	// Step 8b: content-address each supplied input file (name → CAS ref). The
	// paths were already existence/readability-checked in Step 3b; a read error
	// here is an unexpected race (file removed mid-run) and aborts before the
	// log exists. Mirrors the typed-input Put above + the Assets channel.
	inputFileRefs, err := storeInputFiles(blobs, inputFilePaths)
	if err != nil {
		return reportStateFailure(stderr, "awf run", "store run input files", *stateDir, blobsDir, err, r.stateIdentity(), stateFailureInfra)
	}

	assetSnapshots, err := engine.StoreRunStartedAssetsForLoadedDefinition(blobs, ld)
	if err != nil {
		return reportStateFailure(stderr, "awf run", "store workflow assets", *stateDir, blobsDir, err, r.stateIdentity(), stateFailureInfra)
	}
	// Snapshot the run's full canonical definition into Blobs (view-only). Lets a reader
	// render this run faithfully against the structure it executed against, even after the
	// file is edited (`awf ui`). Written before run.started (content-address-then-pointer-
	// swap); never consulted for resume/pinning — §8 drift is decided against the live file.
	// CAS dedup → one blob per distinct definition.
	definitionRef, err := engine.StoreRunStartedDefinitionSnapshot(blobs, ld)
	if err != nil {
		return reportStateFailure(stderr, "awf run", "store workflow definition", *stateDir, blobsDir, err, r.stateIdentity(), stateFailureInfra)
	}
	// Step 9: OpenLogExclusive atomically claims the run.id.
	runDir := filepath.Join(*stateDir, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return reportStateFailure(stderr, "awf run", "create run directory", *stateDir, runDir, err, r.stateIdentity(), stateFailureInfra)
	}
	logPath := filepath.Join(runDir, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A pre-existing run id is a usage conflict (pick a different --run-id),
			// not an environment failure — stays ExitUsage.
			fprintf(stderr, "awf run: run id %q already exists at %q — use `awf resume` to continue an interrupted run, or pick a different --run-id\n", id, logPath)
			return ExitUsage
		}
		// Any other open-log failure is AWF's own log-file I/O → ExitInfra.
		return reportStateFailure(stderr, "awf run", "open run log", *stateDir, logPath, err, r.stateIdentity(), stateFailureInfra)
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
			return reportStateFailure(stderr, "awf run", "acquire run lock", *stateDir, filepath.Join(runDir, "run.lock"), lockErr, r.stateIdentity(), stateFailureInfra)
		}
		// The run-lock is AWF-owned liveness metadata; a held lock (concurrent
		// driver) or a lock I/O failure is an environment condition → ExitInfra.
		return ExitInfra
	}
	defer lock.Release()

	// Step 10: append run.started + fsync. Backend field carries the selected
	// concrete backend kind so resume can pick the same backend without a flag.
	if autoSelectedNative {
		fprintf(stderr, "awf run: auto-selected native backend (no Docker-only features). Resume restores snapshot: workspace workdirs from a full workdir archive but does not pin the host base environment; use --backend docker for a pinned baseline.\n")
	}
	// Non-silent ignored-key warning: native runs steps directly on the host,
	// so it cannot honor a declared container image or resource limits — it
	// ignores both rather than emulating them. Fires for explicit --backend
	// native too (auto never lands here with either — image-mode and
	// docker-preferred features route to docker above), so native never
	// silently drops a declared key (F5, U4: enumerates every ignored key
	// detected, not just the image).
	if concreteBackendKind == engine.BackendNative {
		var ignored []string
		if workflowHasStaticImage(ld) {
			ignored = append(ignored, "container image(s)")
		}
		if workflowHasResources(ld) {
			ignored = append(ignored, "resources")
		}
		if len(ignored) > 0 {
			fprintf(stderr, "awf run: --backend native ignores declared %s; steps run on the host.\n", strings.Join(ignored, " and "))
		}
	}
	// WS-6a: compute the root workflow's structural digest (topology-only,
	// body-invariant). Root-only: imported modules are not folded in (T6b).
	structuralDigest, err := ld.Workflow.StructuralDigest(ld.ComposeFiles, ld.Assets)
	if err != nil {
		fprintf(stderr, "awf run: compute structural digest: %v\n", err)
		return ExitUsage
	}
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:            id,
		WorkflowDigest:   digest,
		StructuralDigest: structuralDigest,
		WorkflowID:       ld.Workflow.ID,      // slice 6.1 — obs awf.workflow.id (standard §9)
		WorkflowVersion:  ld.Workflow.Version, // slice 6.1 — obs awf.workflow.version
		InputRef:         inputRef,
		Backend:          concreteBackendKind,
		Assets:           assetSnapshots,
		InputFiles:       inputFileRefs,
		LiveHome:         engineLiveHomePin(liveRoot.Pin),
		Runtimes:         resolvedRuntimes,             // Phase 5 slice 5.1
		DefinitionRef:    definitionRef,                // view-only snapshot of the run's canonical definition
		SandboxMode:      nativeSandboxModeOf(backend), // F30; "" for non-native backends
	})
	if err != nil {
		fprintf(stderr, "awf run: marshal run.started: %v\n", err)
		return ExitUsage
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		return reportStateFailure(stderr, "awf run", "append run.started", *stateDir, logPath, err, r.stateIdentity(), stateFailureInfra)
	}
	if err := log.Sync(); err != nil {
		return reportStateFailure(stderr, "awf run", "sync run log after run.started", *stateDir, logPath, err, r.stateIdentity(), stateFailureInfra)
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
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "", *stateDir, false, assetSnapshots, inputFileRefs, broker, liveRoot, &skipTeardown, "")
}
