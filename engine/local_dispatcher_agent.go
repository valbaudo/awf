// engine/local_dispatcher_agent.go — the AgentStep dispatch path. Symmetric
// to runCode (in local_dispatcher.go); separated because the agent path is
// self-contained (~150 LOC) and the two paths don't share helpers. See the
// LocalDispatcher.Run kind-switch in local_dispatcher.go for the entry point.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// runAgent is the agent-step counterpart to runCode. Symmetric in shape;
// returns DispatchResult with one of the outcomes listed above.
//
// Outcome mapping (spec §6 mechanical only):
//   - clean Launch + valid Output → ok
//   - *agent.ErrAdapterNotFound      → internal halt (returned as Run's err
//     with dr.Outcome=""; runAgentStep's dr.Outcome=="" branch returns
//     ("", err) — NO node.failed event; preserves fold integrity)
//   - *agent.ErrInvalidConfig         → permanent_failure (workflow bug)
//   - *agent.ErrUnparseableOutput     → retryable_failure (parse miss)
//   - *agent.ErrAgentLaunch / other  → retryable_failure (transport class)
//   - adapter.Refused (slice 5.3+)   → permanent_failure (policy block)
func (d *LocalDispatcher) runAgent(ctx context.Context, intent NodeIntent, as *ir.AgentStep) (DispatchResult, <-chan container.IOChunk, error) {
	// Defense-in-depth nil check. The conformance harness (Task 12) and the
	// CLI (Task 11) always initialize Resolver to a non-nil empty Registry,
	// so this branch normally cannot fire. It exists for callers that
	// construct LocalDispatcher{} directly without setting Resolver — most
	// code-step-only unit tests in engine/local_dispatcher_test.go do — and
	// for the typed-nil interface gotcha (https://go.dev/doc/faq#nil_error):
	// `d.Resolver == nil` catches only an UNTYPED nil. If a caller assigns
	// a typed `(*agent.Registry)(nil)` to the interface, the check passes
	// false and Lookup panics. Harness/CLI initialization defuses that,
	// but the explicit untyped-nil branch here keeps the code-step path
	// safe regardless of caller hygiene.
	if d.Resolver == nil {
		return DispatchResult{}, nil, &agent.ErrAdapterNotFound{Ref: intent.ResolvedInputs.Uses}
	}
	adapter, ok := d.Resolver.Lookup(intent.ResolvedInputs.Uses)
	if !ok {
		return DispatchResult{}, nil, &agent.ErrAdapterNotFound{Ref: intent.ResolvedInputs.Uses}
	}

	// Resolve container handle (same lookup runCode does). A containerless
	// agent step (empty ref — permitted for Containerless adapters, gated at
	// run-start in cli/runtimes.go) gets the zero Handle; the adapter ignores
	// it (e.g. awf/llm issues a direct HTTP call).
	bare, svcOverride := SplitContainerRef(as.Container)

	// Defense-in-depth: engine.Run callers can bypass the CLI run-start guard,
	// and runtime-created container scopes may not be resolvable until dispatch.
	// Catch a non-containerless adapter with an empty container here — the
	// single chokepoint every agent dispatch passes through — before the zero
	// Handle reaches Backend.Exec, where the failure would be misclassified as
	// retryable and burn the retry budget.
	if bare == "" && !adapter.Capabilities().Containerless {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     &agent.ErrInvalidConfig{Ref: intent.ResolvedInputs.Uses, Reason: "agent runtime requires a container, but the step declares none"},
		}, closedChunks(), nil
	}

	// Defense-in-depth: an engine.Run caller that bypasses the CLI, or a future
	// traversal gap in the run-start guard, could reach here with a non-empty
	// Thread against an adapter that doesn't support engine-threaded conversations
	// (Caps.Threaded == false). Without this guard the thread is silently dropped
	// and Launch proceeds with no history — corrupting the conversation. Catch it
	// here before Launch so the failure is classified correctly (permanent, not
	// retryable) and surfaces the step path.
	if len(intent.ResolvedInputs.Thread) > 0 && !adapter.Capabilities().Threaded {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     &agent.ErrInvalidConfig{Ref: intent.ResolvedInputs.Uses, Reason: fmt.Sprintf("step %q uses continues: (engine-threaded), but adapter %q does not support Threaded conversations", intent.Path, intent.ResolvedInputs.Uses)},
		}, closedChunks(), nil
	}

	if len(intent.ResolvedInputs.ContextEvidence) > 0 && !adapter.Capabilities().ContextEvidence {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     &agent.ErrInvalidConfig{Ref: intent.ResolvedInputs.Uses, Reason: fmt.Sprintf("step %q uses evaluator context evidence, but adapter %q does not support ContextEvidence", intent.Path, intent.ResolvedInputs.Uses)},
		}, closedChunks(), nil
	}

	if intent.IsGateEvaluate && adapter.Capabilities().PersistentSession {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     &agent.ErrInvalidConfig{Ref: intent.ResolvedInputs.Uses, Reason: fmt.Sprintf("step %q is in gate.evaluate, but adapter %q declares PersistentSession", intent.Path, intent.ResolvedInputs.Uses)},
		}, closedChunks(), nil
	}

	var h container.Handle
	if bare != "" {
		var ok bool
		handleKey := QualifiedContainerKey(d.RuntimeParent, bare)
		h, ok = d.Handles[handleKey]
		if !ok {
			return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher.runAgent: no handle for container %q (bare %q, key %q) at path %q", as.Container, bare, handleKey, intent.Path)
		}
		if svcOverride != "" {
			h.Service = svcOverride
		}
	}

	// Session restore on the generator frontier (Task 1.5 / M1): before launching
	// the agent, write the committed session `projects/` subtree back into the
	// container so the agent resumes from it. Symmetric inverse of the capture
	// block below.
	//
	// Conditions: SessionDir set + RunState present + a committed ref exists for
	// this node path. All three must hold; any absent → skip (first run or
	// non-session step). The committed blob is a gzip-tar of the subtree; WriteTreeAt
	// extracts it under SessionDir. A Blobs.Get error or WriteTreeAt error is a
	// mechanical failure (crash ≠ verdict) — never commits and never consumes a
	// gate attempt.
	var sessionRestored bool
	if intent.ResolvedInputs.SessionDir != "" && d.RunState != nil && d.Blobs != nil {
		if ref := d.RunState.SessionRefs[intent.Path]; ref != "" {
			sessionBytes, getErr := d.Blobs.Get(ref)
			if getErr != nil {
				oc := snapshotFailureOutcome(getErr)
				return DispatchResult{
					Outcome: oc,
					Err:     fmt.Errorf("engine.LocalDispatcher.runAgent: get session blob %q for %q: %w", ref, intent.Path, getErr),
				}, closedChunks(), nil
			}
			if writeErr := d.Backend.WriteTreeAt(ctx, h, intent.ResolvedInputs.SessionDir, sessionBytes); writeErr != nil {
				oc := snapshotFailureOutcome(writeErr)
				return DispatchResult{
					Outcome: oc,
					Err:     fmt.Errorf("engine.LocalDispatcher.runAgent: restore session subtree at %q for %q: %w", intent.ResolvedInputs.SessionDir, intent.Path, writeErr),
				}, closedChunks(), nil
			}
			sessionRestored = true
		}
	}

	// Reject input_files on a containerless agent step (no container to stage
	// into). The interpreter (engine/agent_step.go) already rejects this BEFORE
	// resolution; this guard is defense-in-depth for a runtime bypass — a direct
	// engine.Run caller skipping the interpreter, or a map-body step. Then stage
	// via the shared helper (mirrors runCode; failure → retryable).
	if len(intent.ResolvedInputs.InputFiles) > 0 && bare == "" {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     fmt.Errorf("engine.LocalDispatcher.runAgent: input_files requires a container at %q", intent.Path),
		}, closedChunks(), nil
	}
	if err := d.stageInputFiles(ctx, h, intent.ResolvedInputs.InputFiles, intent.Path); err != nil {
		return DispatchResult{Outcome: OutcomeRetryableFailure, Err: err}, closedChunks(), nil
	}

	// Declared output_files require a container to capture from. Reject before
	// Launch so a malformed containerless workflow cannot perform agent-side
	// effects and only then fail at capture time.
	if len(intent.ResolvedInputs.OutputFiles) > 0 && bare == "" {
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     fmt.Errorf("engine.LocalDispatcher.runAgent: output_files requires a container at %q", intent.Path),
		}, closedChunks(), nil
	}

	// Apply step timeout to ctx (mirrors runCode).
	var cancel context.CancelFunc
	if intent.ResolvedInputs.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, intent.ResolvedInputs.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Idle timeout: arm a timer that cancels ctx if no AgentEvent is drained for
	// IdleTimeout. Armed here (the pre-first-output window counts, so a step that
	// emits nothing still fires), reset on each event in the drain loop below, and
	// stopped on normal completion. An idle fire cancels ctx exactly like a wall
	// expiry → the adapter surfaces ctx.Err() → retryable_failure → existing retry.
	var idleTimer *time.Timer
	if intent.ResolvedInputs.IdleTimeout > 0 {
		// StartupGrace (D3), when set, is the one-time initial idle window used until
		// the first event is drained (a Coarse adapter's warmup may exceed the
		// per-gap IdleTimeout); the drain loop below switches to IdleTimeout after
		// that first event.
		initialIdle := intent.ResolvedInputs.IdleTimeout
		if intent.ResolvedInputs.StartupGrace > 0 {
			initialIdle = intent.ResolvedInputs.StartupGrace
		}
		idleTimer = time.AfterFunc(initialIdle, cancel)
		defer idleTimer.Stop()
	}

	// Validate config defensively — the run-start walk (U1, cli/with_guard.go's
	// checkWithConfigForLoadedDefinition) already validated every agent step's
	// with: before the log opened; this is the double-check at dispatch time per
	// the Phase 5 design slice 5.2 row "Calls Adapter.ValidateConfig (defensively)".
	if err := adapter.ValidateConfig(intent.ResolvedInputs.With); err != nil {
		// Permanent failure: bad config won't fix itself on retry.
		return DispatchResult{
			Outcome: OutcomePermanentFailure,
			Err:     err,
		}, nil, nil
	}

	// Compute the absolute CLAUDE_CONFIG_DIR for claude-family adapters
	// (IsolatedConfigDir) and thread it via the invocation, so the adapter sets it
	// on the exec env BEFORE Launch (per-run config isolation). The value must be
	// ABSOLUTE: docker/fake StagingRoot is already absolute (use it as-is); native's
	// StagingRoot is workdir-relative, so resolve it under the workdir via
	// WorkdirResolver. Empty for non-claude-family steps. Set for BOTH the base
	// (non-session) adapter and the session adapter; the session subtree
	// capture/restore below is the additional PersistentSession-only behavior.
	var sessionConfigDir string
	if intent.ResolvedInputs.SessionConfigDirRel != "" && d.Backend != nil {
		stagingRoot := d.Backend.Capabilities().StagingRoot
		sessionParent := intent.ResolvedInputs.SessionConfigDirRel
		switch {
		case filepath.IsAbs(stagingRoot):
			sessionConfigDir = sessionParent // docker / fake: StagingRoot already absolute
		default:
			wr, ok := d.Backend.(container.WorkdirResolver)
			if !ok {
				// Fail closed: a relative StagingRoot with no WorkdirResolver cannot
				// yield an ABSOLUTE CLAUDE_CONFIG_DIR, and a relative one would
				// silently point claude at the wrong config dir. Unreachable today
				// (native implements WorkdirResolver; docker/fake StagingRoot is
				// absolute), but never emit a wrong config dir.
				return DispatchResult{
					Outcome: OutcomePermanentFailure,
					Err: fmt.Errorf("engine.LocalDispatcher.runAgent: cannot resolve absolute CLAUDE_CONFIG_DIR for %q: backend StagingRoot %q is relative but backend does not implement container.WorkdirResolver",
						intent.Path, stagingRoot),
				}, nil, nil
			}
			sessionConfigDir = wr.ResolveWorkdirPath(h, sessionParent) // native: resolve under the workdir
		}
	}

	// Build AgentInvocation. Feedback (slice 5.3) carries the prior evaluator
	// verdict on gate repair attempts N>1 — populated by runAgentStep from
	// the enclosing gate's runstate.LookupGateAttempts. Adapters that consume
	// an implicit "previous verdict:" preamble (the Claude Code adapter) read
	// it; nil on attempt 1, non-gate paths, and code steps.
	inv := agent.AgentInvocation{
		NodePath:         intent.Path,
		Uses:             intent.ResolvedInputs.Uses,
		RunContext:       intent.RunContext,
		With:             intent.ResolvedInputs.With,
		OutputSchema:     intent.ResolvedInputs.OutputSchema,
		IdempotencyKey:   intent.IdempotencyKey,
		Feedback:         intent.ResolvedInputs.Feedback, // slice 5.3
		Thread:           intent.ResolvedInputs.Thread,   // Task 4.5
		ContextEvidence:  intent.ResolvedInputs.ContextEvidence,
		InputFiles:       intent.ResolvedInputs.ContainerlessFiles, // resolved input_files for containerless steps; nil for container-backed (those use stageInputFiles)
		Attempt:          intent.Attempt,                           // R1: 1-based per-attempt signal from RunWithRetry
		RecoveryContinue: intent.RecoveryContinue,                  // R3: resolved recovery == continue (session adapters resume on retry)
		ResumeSession:    sessionRestored,                          // M2 task: true when session subtree was written back for this node
		SessionConfigDir: sessionConfigDir,                         // absolute per-run CLAUDE_CONFIG_DIR; adapter sets it on the exec env
		WorkflowDir:      intent.ResolvedInputs.WorkflowDir,        // absolute workflow-file directory; codexlive defaults `cwd` to it (F33)
	}

	// γ contract: Launch returns immediately with events + outcome channels
	// open. Spawn a goroutine that drains events (writing tap lines per event
	// as they arrive — THE REALTIME UX). Main path awaits launchOutcome.
	//
	// Naming: dispatchOutcome (engine's mechanical Outcome enum) vs
	// launchOutcome (γ's AgentOutcome envelope). Two different types — keep
	// names disambiguated.
	events, outcomeCh, launchErr := adapter.Launch(ctx, h, inv)
	if launchErr != nil {
		if errors.Is(launchErr, agent.ErrLiveReplayRequired) {
			return DispatchResult{Err: launchErr}, nil, launchErr
		}
		dispatchOutcome := classifyAgentLaunchErr(launchErr)
		return DispatchResult{Outcome: dispatchOutcome, Err: launchErr}, nil, nil
	}

	type drainedAgentEvents struct {
		events []agent.AgentEvent
		err    error
	}
	drainDone := make(chan drainedAgentEvents, 1)
	go func() {
		var buf []agent.AgentEvent
		var sinkErr error
		render := d.eventRenderer()
		// idleReset is the value each drained event resets the idle timer to. It
		// starts at the initial arm (StartupGrace when set, matching the timer above)
		// and switches to IdleTimeout after the first event proves the turn is
		// producing output — so the startup grace applies only through warmup.
		idleReset := intent.ResolvedInputs.IdleTimeout
		if intent.ResolvedInputs.StartupGrace > 0 {
			idleReset = intent.ResolvedInputs.StartupGrace
		}
		firstEvent := true
		for ev := range events {
			// Any drained event is progress — reset the idle deadline.
			if idleTimer != nil {
				idleTimer.Reset(idleReset)
			}
			if firstEvent {
				idleReset = intent.ResolvedInputs.IdleTimeout
				firstEvent = false
			}
			if d.AgentEventTap != nil {
				render(d.AgentEventTap, ev)
			}
			if ev.Live && intent.agentEventSink != nil {
				if err := intent.agentEventSink.send(ev); err != nil {
					if sinkErr == nil {
						sinkErr = err
						cancel()
					}
				}
				continue
			}
			// Display is json:"-" and never serialized directly. Keep it in memory
			// so the interpreter can copy sanitized scalar summaries for live events.
			buf = append(buf, ev)
		}
		// Events channel closed = agent finished; disarm so a post-completion idle
		// fire can't cancel a step that already produced its outcome.
		if idleTimer != nil {
			idleTimer.Stop()
		}
		drainDone <- drainedAgentEvents{events: buf, err: sinkErr}
	}()

	launchOutcome := <-outcomeCh
	drained := <-drainDone
	bufferedEvents := drained.events
	if drained.err != nil {
		return DispatchResult{
			Err:         drained.err,
			AgentEvents: bufferedEvents,
		}, closedChunks(), drained.err
	}

	// In-flight failure surfaced via launchOutcome.Err.
	if launchOutcome.Err != nil {
		if errors.Is(launchOutcome.Err, agent.ErrLiveReplayRequired) {
			// A stalled PersistentSession turn needing a replay. Propagate the signal:
			// under recovery:continue RunWithRetry retries in-process (the adapter
			// re-derives its session key and continues the live thread); otherwise it
			// halts and a later `awf resume` re-derives defaultSessionKey(runID,nodePath)
			// to read the on-disk live.SessionRecord — no journaled hint needed.
			return DispatchResult{
				Err:         launchOutcome.Err,
				AgentEvents: bufferedEvents,
			}, closedChunks(), launchOutcome.Err
		}
		dispatchOutcome := classifyAgentLaunchErr(launchOutcome.Err)
		return DispatchResult{
			Outcome:     dispatchOutcome,
			Err:         launchOutcome.Err,
			AgentEvents: bufferedEvents,
			RetryAfter:  agentRetryAfter(launchOutcome.Err),
		}, closedChunks(), nil
	}

	// Validate Output against OutputSchema (if declared).
	if intent.ResolvedInputs.OutputSchema != nil {
		if err := ValidateOutputMap(launchOutcome.Result.Output, intent.ResolvedInputs.OutputSchema); err != nil {
			if launchOutcome.Result.Live != nil {
				// A live turn that completed but whose output failed schema validation
				// needs a replay. Signal ErrLiveReplayRequired: under recovery:continue
				// RunWithRetry retries in-process; otherwise a later `awf resume`
				// re-derives defaultSessionKey(runID,nodePath) to read the on-disk
				// live.SessionRecord — no journaled hint needed.
				return DispatchResult{
					Outputs:     launchOutcome.Result.Output,
					AgentEvents: bufferedEvents,
					Err:         agent.ErrLiveReplayRequired,
				}, closedChunks(), agent.ErrLiveReplayRequired
			}
			return DispatchResult{
				Outcome:     OutcomeRetryableFailure,
				Outputs:     launchOutcome.Result.Output,
				AgentEvents: bufferedEvents,
				Err:         &agent.ErrUnparseableOutput{NodePath: intent.Path},
			}, closedChunks(), nil
		}
	}

	// output_artifact (B1): a containerless agent step serializes its validated
	// typed Output as canonical JSON into Files[name], so it flows through the
	// normal artifact machinery. The container-capture output_files rejection
	// above (bare=="" guard, line ~163) is untouched — output_artifact is a
	// distinct field. init-then-insert (never replace: a future adapter may
	// return its own Files); skip when Output is nil (null artifact is useless
	// and defends against a validator bypass).
	if bare == "" && intent.ResolvedInputs.OutputArtifact != "" && launchOutcome.Result.Output != nil {
		blob, mErr := marshalCanonicalJSON(launchOutcome.Result.Output)
		if mErr != nil {
			return DispatchResult{
				Outcome: OutcomePermanentFailure,
				Err:     fmt.Errorf("engine.LocalDispatcher.runAgent: marshal output_artifact at %q: %w", intent.Path, mErr),
			}, closedChunks(), nil
		}
		if launchOutcome.Result.Files == nil {
			launchOutcome.Result.Files = map[string][]byte{}
		}
		launchOutcome.Result.Files[intent.ResolvedInputs.OutputArtifact] = blob
	}

	files := packFiles(launchOutcome.Result.Files)
	if len(intent.ResolvedInputs.OutputFiles) > 0 {
		captured, captureErr := d.Backend.CaptureFiles(ctx, h, intent.ResolvedInputs.OutputFiles)
		if captureErr != nil {
			return DispatchResult{
				Outcome:     OutcomeRetryableFailure,
				Outputs:     launchOutcome.Result.Output,
				AgentEvents: bufferedEvents,
				Err:         captureErr,
			}, closedChunks(), nil
		}
		if err := validateCapturedArtifacts(captured, intent.ResolvedInputs.OutputFileContracts); err != nil {
			return DispatchResult{
				Outcome:     OutcomeRetryableFailure,
				Outputs:     launchOutcome.Result.Output,
				AgentEvents: bufferedEvents,
				Err:         err,
			}, closedChunks(), nil
		}
		files = append(files, captured...)
	}

	exitCodePtr := copyIntPtr(launchOutcome.Result.ExitCode)
	metrics := launchOutcome.Result.Metrics
	if d.StepCostLine && d.AgentEventTap != nil {
		writeAgentCostLine(d.AgentEventTap, intent.Path, metrics)
	}
	dr := DispatchResult{
		Outcome:     OutcomeOK,
		ExitCode:    exitCodePtr,
		Outputs:     launchOutcome.Result.Output,
		AgentEvents: bufferedEvents,
		Files:       files,
		Metrics:     &metrics,
		Transcript:  launchOutcome.Result.Transcript, // adapter-provided; no With["prompt"] read anywhere in engine
	}
	if launchOutcome.Result.Live != nil {
		live := launchOutcome.Result.Live
		dr.Live = &LiveDispatchRecord{
			AdapterRef:     live.AdapterRef,
			SessionKey:     live.SessionKey,
			SessionKeyHash: live.SessionKeyHash,
			LeaseID:        live.LeaseID,
			ActiveTurnID:   live.ActiveTurnID,
			ProviderTurnID: live.ProviderTurnID,
			RunID:          live.RunID,
			NodePath:       live.NodePath,
			Epoch:          live.Epoch,
			CommittedUnix:  live.CommittedUnix,
		}
	}

	// Record the container for OK (committed) steps; failed steps carry none
	// (decision 3: node.completed.container is recorded only for committed/OK
	// steps — obs reads it off every committed step's node.completed).
	if dr.Outcome == OutcomeOK {
		dr.Container = QualifiedContainerKey(d.RuntimeParent, bare)
	}
	// snapshot:workspace capture (slice 7.1) — identical to runCode. Only an OK
	// step that committed records its container; a failed launch returned
	// earlier without it. Capture the CoW workspace diff after success;
	// terminal failure → permanent_failure (Phase-4 decision 11), transient →
	// retryable, classified behind the container seam via errors.Is.
	if dr.Outcome == OutcomeOK && intent.ResolvedInputs.Snapshot == "workspace" {
		ref, snapErr := d.Backend.Snapshot(ctx, h)
		if snapErr != nil {
			oc := snapshotFailureOutcome(snapErr)
			return DispatchResult{
				Outcome:     oc,
				ExitCode:    exitCodePtr,
				AgentEvents: bufferedEvents,
				Err:         fmt.Errorf("engine.LocalDispatcher.runAgent: snapshot %q at %q: %w", QualifiedContainerKey(d.RuntimeParent, bare), intent.Path, snapErr),
			}, closedChunks(), nil
		}
		dr.SnapshotRef = string(ref)
	}
	if dr.Outcome == OutcomeOK && intent.ResolvedInputs.SessionDir != "" {
		transcript, readErr := d.Backend.ReadTreeAt(ctx, h, intent.ResolvedInputs.SessionDir)
		if readErr != nil {
			oc := snapshotFailureOutcome(readErr)
			return DispatchResult{
				Outcome:     oc,
				ExitCode:    exitCodePtr,
				AgentEvents: bufferedEvents,
				Err:         fmt.Errorf("engine.LocalDispatcher.runAgent: capture session %q at %q: %w", QualifiedContainerKey(d.RuntimeParent, bare), intent.Path, readErr),
			}, closedChunks(), nil
		}
		dr.SessionTranscript = transcript
	}
	return dr, closedChunks(), nil
}

// classifyAgentLaunchErr maps an Adapter.Launch error to an Outcome per the
// Adapter contract (agent/adapter.go doc on Launch). Defaults to
// retryable_failure for transport-class faults.
func classifyAgentLaunchErr(err error) Outcome {
	var notFound *agent.ErrAdapterNotFound
	var badConfig *agent.ErrInvalidConfig
	var unparseable *agent.ErrUnparseableOutput
	switch {
	// Defense-in-depth: *ErrAdapterNotFound is normally caught before Launch by
	// the Resolver.Lookup miss path in runAgent. This branch handles the unlikely
	// case of an adapter whose Launch surfaces a wrapped *ErrAdapterNotFound,
	// violating the agent/adapter.go Launch contract. Classification still maps
	// to permanent_failure — a broken adapter contract is not transient.
	case errors.As(err, &notFound):
		return OutcomePermanentFailure
	case errors.As(err, &badConfig):
		return OutcomePermanentFailure
	case errors.Is(err, agent.ErrPermissionDenied):
		return OutcomePermanentFailure
	case errors.As(err, &unparseable):
		return OutcomeRetryableFailure
	default:
		// *agent.ErrAgentLaunch and any other error class → transport.
		return OutcomeRetryableFailure
	}
}

// agentRetryAfter extracts an adapter-supplied Retry-After hint from a launch
// error, if present, so the dispatcher can surface it on
// DispatchResult.RetryAfter for engine.RunWithRetry to honor. Returns 0 when
// the error is not an *agent.ErrAgentLaunch or carries no RetryHint. The engine
// never reads HTTP — adapters parse the provider signal and attach the typed
// hint; this only unwraps it.
func agentRetryAfter(err error) time.Duration {
	var la *agent.ErrAgentLaunch
	if errors.As(err, &la) && la.RetryHint != nil {
		return la.RetryHint.RetryAfter
	}
	return 0
}

// closedChunks returns a pre-closed IOChunk channel — runAgent doesn't emit
// IOChunks (agent events are a separate stream), but the Dispatcher.Run
// contract guarantees a non-nil channel on a successful return so callers
// can `for range` safely without nil-checking.
func closedChunks() <-chan container.IOChunk {
	ch := make(chan container.IOChunk)
	close(ch)
	return ch
}

// packFiles converts agent.AgentResult.Files (map[path][]byte) into the
// container.CapturedFile shape DispatchResult.Files expects.
func packFiles(files map[string][]byte) []container.CapturedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]container.CapturedFile, 0, len(files))
	for path, content := range files {
		out = append(out, container.CapturedFile{Path: path, Content: content})
	}
	return out
}

// eventRenderer returns the live-tap formatter: the injected RenderAgentEvent if
// set, else the built-in terse fallback. Never nil.
func (d *LocalDispatcher) eventRenderer() func(io.Writer, agent.AgentEvent) {
	if d.RenderAgentEvent != nil {
		return d.RenderAgentEvent
	}
	return writeAgentEventTap
}

// writeAgentCostLine emits one per-step cost/token summary line to the live tap.
// Best-effort (write errors ignored, like writeAgentEventTap). Sourced from the
// adapter's MetricSet — no harness parsing, no obs.
func writeAgentCostLine(w io.Writer, path string, m agent.MetricSet) {
	_, _ = fmt.Fprintf(w, "%s %s — $%.4f · %d tok (%d in / %d out) · %d turns\n",
		"·", path, m.Cost.Total, m.Tokens.Input+m.Tokens.Output, m.Tokens.Input, m.Tokens.Output, m.Turns)
}

// writeAgentEventTap emits one line per AgentEvent to the tap writer.
// Write errors are intentionally ignored — the tap is best-effort stderr
// output; a write failure must not abort the dispatch attempt.
func writeAgentEventTap(w io.Writer, ev agent.AgentEvent) {
	const maxPayloadPreview = 80
	preview := ev.Payload
	if len(preview) > maxPayloadPreview {
		preview = preview[:maxPayloadPreview]
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", ev.Kind, preview)
}
