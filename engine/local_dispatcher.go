package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// LocalDispatcher is the Phase 2 Dispatcher impl — runs code steps in-process
// via container.Backend, returns DispatchResult with mechanical Outcome.
// AgentStep is dispatched via runAgent (slice 5.2+); SignalStep returns
// ErrUnsupportedKind so the interpreter halts cleanly.
//
// The dispatcher OWNS NO state: it doesn't Create / Destroy containers
// (the interpreter does, slice 2.5), it doesn't write to Log / Blobs
// (engine.Commit does), it doesn't loop on failure (engine.RunWithRetry
// does). One attempt in, one DispatchResult out.
//
// Handles is the declared-container-name → pre-Created container.Handle map.
// The interpreter Creates each handle before the first dispatch into it and
// Destroys at run-end / cancellation. Lookup-miss is a hard error (not a
// retryable failure) — it indicates an interpreter bug.
type LocalDispatcher struct {
	Backend container.Backend
	Handles map[string]container.Handle
	// RuntimeParent scopes declared container names for call workflows. Empty
	// means root workflow; non-empty handles are keyed as
	// QualifiedContainerKey(RuntimeParent, container).
	RuntimeParent string
	// ComposeFiles is the workflow's compose-file bytes by workflow-relative
	// path — sourced from ir.LoadedDefinition.ComposeFiles at construction.
	// engine/map.go reads this when Creating per-item containers for compose-
	// mode `containers:` entries; image-mode entries ignore it. Nil-safe (an
	// image-mode-only workflow may leave this unset).
	ComposeFiles map[string][]byte

	// Slice 5.2 — agent-step seam.
	//
	// Resolver is the agent.Resolver the dispatcher consults to look up
	// agent adapters by AgentStep.Uses. Nil means no agents registered —
	// any AgentStep dispatch returns *agent.ErrAdapterNotFound (mapped to
	// permanent_failure: the workflow references a `uses:` ref the
	// operator didn't register). Production wiring lives in cli/execute.go
	// (Task 11); tests inject *agent.Registry directly.
	//
	// Concurrency: Resolver.Lookup MUST be safe for concurrent use (Phase
	// 3 parallel branches dispatch concurrently). agent.Registry from
	// slice 5.1 satisfies this via sync.RWMutex.
	Resolver agent.Resolver

	// AgentEventTap is the io.Writer the dispatcher writes one-line-per-
	// AgentEvent stderr output to. Nil disables the live tap entirely
	// (tests). The CLI sets this to stderr (Task 11). Separate from the
	// io.Writer threaded through engine.Run (which is for IOChunks) so
	// agent events render with their own per-kind formatting without
	// interleaving with code-step stdout.
	AgentEventTap io.Writer

	// RenderAgentEvent formats each live AgentEvent to the AgentEventTap writer.
	// nil → the built-in terse fallback (writeAgentEventTap). cli injects a
	// TTY-aware, per-kind renderer; the engine stays presentation-agnostic.
	RenderAgentEvent func(io.Writer, agent.AgentEvent)

	// StepCostLine, when true, makes runAgent write one cost/token line per
	// agent step to AgentEventTap (slice 6.2 live renderer). Default false so
	// engine unit tests' exact tap-write-count assertions are unaffected; the
	// CLI (cli/execute.go) sets it true. Sourced from the adapter's already-
	// extracted MetricSet — never parses harness output, never touches obs.
	StepCostLine bool

	// M1 session restore — read-only access for the agent restore site.
	//
	// RunState is the folded run state for the current run. When non-nil and
	// RunState.SessionRefs[path] is set, runAgent restores the committed session
	// transcript into the container BEFORE launching the agent (the inverse of the
	// capture block). Nil means no prior session map (first run or non-session step).
	// The interpreter sets this in interpreter.go (engine.Run's LocalDispatcher
	// construction); direct LocalDispatcher callers (tests) can set it too.
	//
	// NOTE: the dispatcher NEVER writes to RunState; only the interpreter (via
	// engine.Commit + runstate.RecordCompleted) updates it. The restore path only
	// calls RunState.SessionRefs (read) + Blobs.Get (read) + Backend.WriteFileAt.
	RunState *RunState

	// Blobs is the shared content-addressed store. Read-only from the dispatcher:
	// the restore path calls Blobs.Get to materialize the committed session transcript.
	// Only engine.Commit (interpreter-side) calls Blobs.Put — never the dispatcher.
	Blobs state.Blobs
}

// Run executes one attempt of intent.Node. See the Dispatcher interface doc
// for the IOChunk channel contract; in particular, on a non-nil returned
// error the channel is nil (callers MUST check err first).
func (d *LocalDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	switch n := intent.Node.(type) {
	case *ir.CodeStep:
		return d.runCode(ctx, intent, n)
	case *ir.AgentStep:
		return d.runAgent(ctx, intent, n)
	case *ir.SignalStep:
		return DispatchResult{}, nil, fmt.Errorf("%w (path=%s)", ErrUnsupportedKind, intent.Path)
	default:
		return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher: non-step node at path %q (control nodes don't reach the dispatcher — interpreter bug)", intent.Path)
	}
}

// stageInputFiles copies resolved input_files into the container before
// exec/launch (the SP1 artifact channel), OUTSIDE the step timeout (staging is
// setup, not the step's work). Returns nil when there are none or on success;
// a wrapped error otherwise — the caller maps it to a RETRYABLE failure (the
// bytes already exist in Blobs; the container may be transiently unwritable).
// Staged on every attempt (retry = identical inputs); CopyTo overwrites
// idempotently. Shared by runCode and runAgent.
func (d *LocalDispatcher) stageInputFiles(ctx context.Context, h container.Handle, files []container.InputFile, path string) error {
	if len(files) == 0 {
		return nil
	}
	if err := d.Backend.CopyTo(ctx, h, files); err != nil {
		return fmt.Errorf("engine.LocalDispatcher: stage input_files at %q: %w", path, err)
	}
	return nil
}

func (d *LocalDispatcher) runCode(ctx context.Context, intent NodeIntent, cs *ir.CodeStep) (DispatchResult, <-chan container.IOChunk, error) {
	bare, svcOverride := SplitContainerRef(cs.Container)
	var handleKey string
	if bare == "" {
		// F4a: a bare `run:` step (no declared container:) resolves to its
		// per-step implicit host-workspace handle instead of
		// QualifiedContainerKey(d.RuntimeParent, "") — a key that's never
		// populated (there's no declared container named ""). The
		// interpreter (runCodeStepWithContext) Creates this handle via
		// hostWorkspaceSpec and registers it under this same key — via
		// WithRawHandle — before dispatch.
		handleKey = BareRunHandleKey(intent.Path)
	} else {
		handleKey = QualifiedContainerKey(d.RuntimeParent, bare)
	}
	h, ok := d.Handles[handleKey]
	if !ok {
		return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher: no handle for container %q (bare name %q, key %q) at path %q (interpreter must Create before dispatch)", cs.Container, bare, handleKey, intent.Path)
	}
	if svcOverride != "" {
		h.Service = svcOverride // shallow clone — h is a Handle value type; no aliasing of d.Handles
	}

	// Code steps always dispatch against SOME handle — either a declared
	// container (the common case) or, since F4a, a bare step's per-step
	// implicit host-workspace handle (empty container:; no containerless
	// guard needed the way runAgent needs one, since the handle above always
	// exists by the time we get here). Stage input_files before exec.
	if err := d.stageInputFiles(ctx, h, intent.ResolvedInputs.InputFiles, intent.Path); err != nil {
		return DispatchResult{Outcome: OutcomeRetryableFailure, Err: err}, nil, nil
	}

	// Apply step timeout(s) to ctx. On expiry the Backend.Exec call observes ctx
	// cancellation and returns an error; ClassifyOutcome then routes it to
	// retryable_failure per spec §6. The wall timeout bounds total elapsed time;
	// the idle timeout (armed here, reset on each IOChunk in the drain loop below)
	// bounds time with no output.
	var cancel context.CancelFunc
	if intent.ResolvedInputs.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, intent.ResolvedInputs.Timeout)
	} else if intent.ResolvedInputs.IdleTimeout > 0 {
		ctx, cancel = context.WithCancel(ctx)
	}
	if cancel != nil {
		defer cancel()
	}
	var idleTimer *time.Timer
	if intent.ResolvedInputs.IdleTimeout > 0 {
		idleTimer = time.AfterFunc(intent.ResolvedInputs.IdleTimeout, cancel)
		defer idleTimer.Stop()
	}

	// Build the env: defensive-copy the caller's map so we don't mutate it,
	// then layer in AWF_IDEMPOTENCY_KEY (slice 2.4, AWF §10) and AWF_OUTPUT
	// (slice 4.2, AWF §4.1 — only when output_schema is declared).
	//
	// Docker daemon merges Env additively (daemon/exec.go:143 via
	// ReplaceOrAppendEnvValues): the container's PATH/HOME/etc. are
	// preserved; our injected vars layer on top.
	env := make(map[string]string, len(intent.ResolvedInputs.Env)+2)
	for k, v := range intent.ResolvedInputs.Env {
		env[k] = v
	}
	if intent.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = intent.IdempotencyKey
	}
	var awfOutputPath string
	if intent.ResolvedInputs.OutputSchema != nil {
		awfOutputPath = awfOutputTempPath(d.Backend.Capabilities().OutputRoot, intent.Path)
		env["AWF_OUTPUT"] = awfOutputPath
	}

	cmd := container.Cmd{
		Run: intent.ResolvedInputs.Command,
		Env: env,
	}

	rawChunks, resultCh, callErr := d.Backend.Exec(ctx, h, cmd)
	// Transport error path — return early with retryable_failure. No artifacts
	// to capture (we don't know what state the container is in).
	if callErr != nil {
		return DispatchResult{
			Outcome: ClassifyOutcome(0, nil, callErr, intent.ResolvedInputs.NonRetryableExitCodes),
			Err:     callErr,
		}, nil, nil
	}

	// Drain rawChunks into a slice BEFORE waiting on resultCh. The
	// forwarder-goroutine pattern (bounded buffer + range-and-forward) deadlocks
	// on long streams: rawChunks fills → Backend's pipe-reader blocks → process
	// can't exit → resultCh never fires → dispatcher hangs forever; the
	// interpreter's drainTap can't help because it doesn't start until
	// dispatcher returns.
	//
	// Drain-to-slice trades streaming UX for correctness: live-tap for
	// CodeSteps is now post-hoc (chunks render after Exec completes). Realtime
	// UX lives on the agent path (Task 7c, Adapter γ contract). CodeStep stdout
	// is typically small; memory cost is bounded.
	var collected []container.IOChunk
	for c := range rawChunks {
		// Any chunk (stdout or stderr) is progress — reset the idle deadline.
		if idleTimer != nil {
			idleTimer.Reset(intent.ResolvedInputs.IdleTimeout)
		}
		collected = append(collected, c)
	}
	if idleTimer != nil {
		idleTimer.Stop()
	}
	exec := <-resultCh

	// ExecResult.Err surfaces transport-class errors (stdcopy mid-stream failure
	// on docker; ctx.Canceled on native). Pre-slice-5.3 these came back via
	// Backend.Exec's err return; the streaming refactor moved them to the
	// result channel.
	if exec.Err != nil {
		return DispatchResult{
			Outcome: ClassifyOutcome(0, nil, exec.Err, intent.ResolvedInputs.NonRetryableExitCodes),
			Err:     exec.Err,
		}, nil, nil
	}

	// Build pre-closed channel for the interpreter's drainTap.
	chunks := make(chan container.IOChunk, len(collected))
	for _, c := range collected {
		chunks <- c
	}
	close(chunks)

	// AWFOutput source selection (slice 4.2 / Design Q1):
	//   * Backend supplied AWFOutput (Phase 2 fake's ProgramExec path) → use directly.
	//   * Backend left AWFOutput nil AND output_schema declared AND exit==0 →
	//     capture the AWF_OUTPUT tempfile (Phase 4 Docker path) and use those bytes.
	// The dispatcher chooses based on what the Backend returned; it does NOT
	// introspect the Backend type.
	awfOutputBytes := exec.AWFOutput
	captureAWFOutput := awfOutputBytes == nil && intent.ResolvedInputs.OutputSchema != nil && exec.ExitCode == 0

	// Build the capture list. AWF_OUTPUT tempfile goes first so we can strip
	// it from the user-visible Files slice after capture.
	filesToCapture := intent.ResolvedInputs.OutputFiles
	if captureAWFOutput {
		filesToCapture = append([]string{awfOutputPath}, filesToCapture...)
	}

	var files []container.CapturedFile
	if exec.ExitCode == 0 && len(filesToCapture) > 0 {
		captured, captureErr := d.Backend.CaptureFiles(ctx, h, filesToCapture)
		if captureErr != nil {
			// A declared output file (or the AWF_OUTPUT tempfile) missing or
			// unreadable is a retryable failure — the step succeeded by exit
			// code but didn't honor its declared contract. We don't attempt
			// schema validation in this branch: the capture failure dominates,
			// and on the Docker path the AWF_OUTPUT bytes themselves may have
			// been the missing file (parse would just be a duplicate "no
			// bytes" error). The previous flow joined parseErr+captureErr;
			// after slice 4.2's restructure parseErr is unset at this point,
			// so the join would be a no-op — captureErr alone is the
			// authoritative cause.
			if captureAWFOutput && len(intent.ResolvedInputs.OutputFiles) == 0 {
				captureErr = fmt.Errorf("code step %q declares output_schema but did not write $AWF_OUTPUT (%s): %w", intent.Path, awfOutputPath, captureErr)
			}
			return DispatchResult{
				Outcome:  OutcomeRetryableFailure,
				ExitCode: copyIntPtr(exec.ExitCode),
				Stdout:   exec.Stdout,
				Err:      captureErr,
			}, chunks, nil
		}
		if captureAWFOutput {
			// First captured file is the AWF_OUTPUT tempfile.
			awfOutputBytes = captured[0].Content
			files = captured[1:]
		} else {
			files = captured
		}
	}
	if err := validateCapturedArtifacts(files, intent.ResolvedInputs.OutputFileContracts); err != nil {
		return DispatchResult{
			Outcome:  OutcomeRetryableFailure,
			ExitCode: copyIntPtr(exec.ExitCode),
			Stdout:   exec.Stdout,
			Err:      err,
		}, chunks, nil
	}

	// Validate AWFOutput against the schema (if any). If no schema is declared,
	// slice 1.4's validator rejects step.<id>.<field> refs (AWF3xxx), so any
	// decoded value would be unreachable — don't decode.
	var outputs map[string]any
	var parseErr error
	if intent.ResolvedInputs.OutputSchema != nil {
		outputs, parseErr = ValidateAgainstSchema(awfOutputBytes, intent.ResolvedInputs.OutputSchema)
	}

	dr := DispatchResult{
		Outcome:  ClassifyOutcome(exec.ExitCode, parseErr, nil, intent.ResolvedInputs.NonRetryableExitCodes),
		ExitCode: copyIntPtr(exec.ExitCode),
		Outputs:  outputs,
		Stdout:   exec.Stdout,
		Files:    files,
	}
	if exec.ExitCode != 0 {
		if parseErr != nil {
			dr.Err = fmt.Errorf("process exited with code %d: %w", exec.ExitCode, parseErr)
		} else {
			dr.Err = fmt.Errorf("process exited with code %d", exec.ExitCode)
		}
	} else if parseErr != nil {
		dr.Err = parseErr
	}

	// Record the container for OK (committed) steps; failed steps carry none
	// (decision 3: node.completed.container is recorded only for committed/OK
	// steps — obs reads it off every committed step's node.completed).
	if dr.Outcome == OutcomeOK {
		dr.Container = handleKey
	}
	// snapshot:workspace capture (slice 7.1). Only an OK step that committed
	// records its container; a failed exec returned earlier without it. After a
	// successful exec, capture the CoW workspace diff (the Backend materializes
	// it in Blobs and returns a ref; the interpreter records it at commit).
	if dr.Outcome == OutcomeOK && intent.ResolvedInputs.Snapshot == "workspace" {
		ref, snapErr := d.Backend.Snapshot(ctx, h)
		if snapErr != nil {
			oc := snapshotFailureOutcome(snapErr)
			return DispatchResult{
				Outcome:  oc,
				ExitCode: copyIntPtr(exec.ExitCode),
				Stdout:   exec.Stdout,
				Err:      fmt.Errorf("engine.LocalDispatcher: snapshot %q at %q: %w", handleKey, intent.Path, snapErr),
			}, chunks, nil
		}
		dr.SnapshotRef = string(ref)
	}
	return dr, chunks, nil
}

// snapshotFailureOutcome classifies a Backend.Snapshot error. A terminal error
// (the diff is too large, or the backend can't snapshot) is a permanent_failure:
// it is deterministic, so retrying re-runs the whole step to fail identically
// (Phase-4 decision 11). Anything else (daemon hiccup, ctx-cancel, I/O) is a
// transient retryable_failure. The terminal predicate is matched behind the
// container seam (errors.Is against container sentinels), never a docker type.
//
// Shared by runCode (here) and runAgent (local_dispatcher_agent.go); both files
// are package engine.
func snapshotFailureOutcome(err error) Outcome {
	if errors.Is(err, container.ErrSnapshotTooLarge) || errors.Is(err, container.ErrUnsupported) {
		return OutcomePermanentFailure
	}
	return OutcomeRetryableFailure
}

func copyIntPtr(v int) *int {
	out := v
	return &out
}

// ContainerSpecFor builds the DTO Backend.Create consumes from the IR + a
// composeFiles lookup. Reads Image + Resources for image-mode (slice 4.1);
// reads Compose bytes + ComposePath + Service for compose-mode (slice 4.3)
// by looking up composeFiles by the container's declared compose path.
//
// Exported (capitalized) so cli/run.go and cli/resume.go can call it during
// container provisioning. The internal caller (engine/map.go) reads
// composeFiles from ld.ComposeFiles directly — see dispatchItem's call site.
// There is only one implementation of "build spec from IR" and it is a
// pure function: output depends only on (wf, composeFiles, name).
//
// wf must be non-nil (caller invariant — the loader always produces a valid
// Workflow before provisioning).
//
// Returns ContainerSpec{Name: name} for an undeclared name (validator
// should have caught this; defensive return).
func ContainerSpecFor(wf *ir.Workflow, composeFiles map[string][]byte, name string) container.ContainerSpec {
	spec := container.ContainerSpec{Name: name}
	c, ok := wf.Containers[name]
	if !ok {
		return spec
	}
	// Image-mode.
	if c.Image != "" {
		spec.Image = c.Image
		spec.Cmd = c.Cmd
		spec.DisableKeepalive = c.Keepalive != nil && !*c.Keepalive
		if c.Resources != nil {
			spec.Resources = &container.ContainerResources{
				CPU: c.Resources.CPU,
				Mem: c.Resources.Mem,
			}
		}
		return spec
	}
	// Compose-mode. Read bytes from composeFiles; if absent (defensive —
	// loader.Load should have populated them), leave Compose nil so the
	// Backend errors with a clear "compose bytes missing" message downstream.
	if c.Compose != "" {
		spec.ComposePath = c.Compose
		spec.Service = c.Service
		if composeFiles != nil {
			if b, ok := composeFiles[c.Compose]; ok {
				spec.Compose = b
			}
		}
		return spec
	}
	// Resources-only (P6a): a container declared solely to receive a map's
	// `image:` carries no static image/compose; the per-element image is set by
	// dispatchItem. Copy any declared resources so the runtime image inherits them.
	if c.Resources != nil {
		spec.Resources = &container.ContainerResources{
			CPU: c.Resources.CPU,
			Mem: c.Resources.Mem,
		}
	}
	return spec
}

// hostWorkspaceSpec builds the container.ContainerSpec for F4a's per-step
// implicit host-workspace handle — a bare `run:` code step with no declared
// container:. It carries no Image and no Compose:
//
//   - native.Backend.Create already ignores spec.Image (native isn't
//     digest-pinned — decision 1) and only rejects spec.Compose != nil (which
//     stays nil here), so it's treated as an ordinary per-container workdir.
//   - container/fake.Create treats a spec with no Image/Compose as an
//     ordinary empty-spec Create: it mints a fresh, isolated in-memory
//     fakeHandle regardless of spec contents (container/fake.go Create),
//     which is exactly the per-step isolation this needs.
//
// spec.Name is a "_run."-prefixed form of the step's node path: deterministic
// (stable across resume — F4b's resume path reuses this same helper) and
// guaranteed distinct from every OTHER Create call's spec.Name in the same
// run. Declared containers (ContainerSpecFor) and map per-item containers
// (engine/map.go dispatchItem, "<container>-item-N") both derive their Name
// from a workflow-declared container name, which ir's containerNamePattern
// forbids from containing '.' — so neither can ever collide with a
// "_run.<nodePath>" name (nodePath always contains '.'/'['/']' from
// ir.PathFor for anything but a single top-level step, and the "_run." prefix
// alone is enough to distinguish even that case). Kept distinct from
// BareRunHandleKey's "_run:" Handles-map key (a Go-map key, unconstrained) —
// spec.Name additionally has to be a safe on-disk directory-name component on
// the native backend, so it avoids ':'.
func hostWorkspaceSpec(nodePath string) container.ContainerSpec {
	return container.ContainerSpec{Name: "_run." + nodePath}
}

// SplitContainerRef parses a step's container reference into a bare name and
// optional service override. The spec §3 form `container: lab:db` addresses
// the `db` service of the `lab` compose project; bare `container: lab` uses
// the default service. Mirrors the validator's split in
// ir/validate_structural.go checkContainerRef (kept parallel by docstring;
// not shared code — see slice 4.3 plan Design Q4).
//
// Splits only the FIRST colon; "lab:db:replica" yields ("lab", "db:replica").
// The Backend further splits or rejects as needed.
func SplitContainerRef(ref string) (bare, service string) {
	if i := strings.Index(ref, ":"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func (d *LocalDispatcher) handleKey(containerRef string) string {
	bare, _ := SplitContainerRef(containerRef)
	return QualifiedContainerKey(d.RuntimeParent, bare)
}

// AgentResolver implements AdapterResolver — exposes the dispatcher's
// agent.Resolver to the interpreter so it can wire ResolvedInputs.SessionDir.
func (d *LocalDispatcher) AgentResolver() agent.Resolver { return d.Resolver }

// WithItemHandle returns a shallow clone of d with Handles cloned and the
// (name → h) entry overridden (or inserted). Slice 3.4: the map executor
// (engine/map.go) calls this per item to retarget body's container lookup
// to a per-item handle. Cheap — Handles is typically 1-3 entries.
//
// The returned LocalDispatcher shares Backend and ComposeFiles with d (both
// are read-only from the dispatcher's perspective — no deep copy needed).
// Mutating the returned dispatcher's Handles after construction is safe (it's
// a fresh map).
func (d *LocalDispatcher) WithItemHandle(name string, h container.Handle) *LocalDispatcher {
	cloned := make(map[string]container.Handle, len(d.Handles)+1)
	for k, v := range d.Handles {
		cloned[k] = v
	}
	cloned[QualifiedContainerKey(d.RuntimeParent, name)] = h
	return &LocalDispatcher{
		Backend:          d.Backend,
		Handles:          cloned,
		RuntimeParent:    d.RuntimeParent,
		ComposeFiles:     d.ComposeFiles,
		Resolver:         d.Resolver,
		AgentEventTap:    d.AgentEventTap,
		RenderAgentEvent: d.RenderAgentEvent,
		StepCostLine:     d.StepCostLine,
		RunState:         d.RunState,
		Blobs:            d.Blobs,
	}
}

// WithRawHandle returns a shallow clone of d with Handles cloned and the
// (key → h) entry overridden (or inserted) VERBATIM — unlike WithItemHandle,
// key is NOT run through QualifiedContainerKey(d.RuntimeParent, key). F4a's
// per-step implicit host-workspace handle uses this: key is
// BareRunHandleKey(nodePath), already a globally-unique reserved token (the
// node path is itself already call-workflow-qualified — see
// BareRunHandleKey's doc comment), not a bare workflow-declared container
// name that still needs RuntimeParent qualification.
//
// Same immutable-clone contract as WithItemHandle: d.Handles itself is never
// mutated, so concurrent callers (Parallel branches dispatching bare steps
// concurrently — engine/parallel.go shares one ictx.dispatcher across
// branches) each get their own clone with no data race on the shared map.
func (d *LocalDispatcher) WithRawHandle(key string, h container.Handle) *LocalDispatcher {
	cloned := make(map[string]container.Handle, len(d.Handles)+1)
	for k, v := range d.Handles {
		cloned[k] = v
	}
	cloned[key] = h
	return &LocalDispatcher{
		Backend:          d.Backend,
		Handles:          cloned,
		RuntimeParent:    d.RuntimeParent,
		ComposeFiles:     d.ComposeFiles,
		Resolver:         d.Resolver,
		AgentEventTap:    d.AgentEventTap,
		RenderAgentEvent: d.RenderAgentEvent,
		StepCostLine:     d.StepCostLine,
		RunState:         d.RunState,
		Blobs:            d.Blobs,
	}
}
