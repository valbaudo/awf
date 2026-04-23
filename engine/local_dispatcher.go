package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// LocalDispatcher is the Phase 2 Dispatcher impl — runs code steps in-process
// via container.Backend, returns DispatchResult with mechanical Outcome.
// Agent / Signal steps return ErrUnsupportedKind so the interpreter halts
// cleanly with a "this kind lands in a later phase" message.
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
}

// Run executes one attempt of intent.Node. See the Dispatcher interface doc
// for the IOChunk channel contract; in particular, on a non-nil returned
// error the channel is nil (callers MUST check err first).
func (d *LocalDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	switch n := intent.Node.(type) {
	case *ir.CodeStep:
		return d.runCode(ctx, intent, n)
	case *ir.AgentStep, *ir.SignalStep:
		return DispatchResult{}, nil, fmt.Errorf("%w (path=%s)", ErrUnsupportedKind, intent.Path)
	default:
		return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher: non-step node at path %q (control nodes don't reach the dispatcher — interpreter bug)", intent.Path)
	}
}

func (d *LocalDispatcher) runCode(ctx context.Context, intent NodeIntent, cs *ir.CodeStep) (DispatchResult, <-chan container.IOChunk, error) {
	h, ok := d.Handles[cs.Container]
	if !ok {
		return DispatchResult{}, nil, fmt.Errorf("engine.LocalDispatcher: no handle for container %q at path %q (interpreter must Create before dispatch)", cs.Container, intent.Path)
	}

	// Apply step timeout to ctx (if any). On expiry the Backend.Exec call
	// observes ctx cancellation and returns an error; ClassifyOutcome then
	// routes it to retryable_failure per spec §6.
	if intent.ResolvedInputs.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, intent.ResolvedInputs.Timeout)
		defer cancel()
	}

	// Build the env: defensive-copy the caller's map so we don't mutate it;
	// add AWF_IDEMPOTENCY_KEY iff IdempotencyKey is non-empty (per AWF §10).
	//
	// We deliberately do NOT set AWF_OUTPUT here (Revision #4): the Phase 2
	// fake doesn't read env, and the Docker Backend (Phase 4) owns the
	// $AWF_OUTPUT tempfile path because it chooses where to mount/read it.
	// Setting it here in Phase 2 would be dead code AND would pre-empt
	// Phase 4's design decision.
	env := make(map[string]string, len(intent.ResolvedInputs.Env)+1)
	for k, v := range intent.ResolvedInputs.Env {
		env[k] = v
	}
	if intent.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = intent.IdempotencyKey
	}

	cmd := container.Cmd{
		Run: intent.ResolvedInputs.Command,
		Env: env,
	}

	exec, chunks, callErr := d.Backend.Exec(ctx, h, cmd)
	// Transport error path — return early with retryable_failure. No artifacts
	// to capture (we don't know what state the container is in).
	if callErr != nil {
		return DispatchResult{
			Outcome: ClassifyOutcome(0, nil, callErr, intent.ResolvedInputs.NonRetryableExitCodes),
			Err:     callErr,
		}, nil, nil
	}

	// Parse $AWF_OUTPUT against the step's schema (if any). If no schema is
	// declared, we do NOT decode AWFOutput — slice 1.4's validator rejects
	// step.<id>.<field> refs without a producer schema (AWF3xxx), so any
	// decoded value would be unreachable. Decoding it would be write
	// amplification + silently swallowing decode errors (Revision #7).
	var outputs map[string]any
	var parseErr error
	if intent.ResolvedInputs.OutputSchema != nil {
		outputs, parseErr = ValidateAgainstSchema(exec.AWFOutput, intent.ResolvedInputs.OutputSchema)
	}

	// Capture output_files (only if the step exited 0; otherwise the files may
	// not exist or may be in a torn state).
	var files []container.CapturedFile
	var captureErr error
	if exec.ExitCode == 0 && len(intent.ResolvedInputs.OutputFiles) > 0 {
		files, captureErr = d.Backend.CaptureFiles(ctx, h, intent.ResolvedInputs.OutputFiles)
		if captureErr != nil {
			// A declared output file missing or unreadable is a retryable failure
			// — same class as an unparseable AWFOutput (the step succeeded by
			// exit code but didn't honor its declared contract). If parseErr is
			// also non-nil (schema validation failed AND capture failed), join
			// both so the operator sees the full failure picture rather than
			// only the last symptom.
			return DispatchResult{
				Outcome:  OutcomeRetryableFailure,
				ExitCode: copyIntPtr(exec.ExitCode),
				Outputs:  outputs,
				Stdout:   exec.Stdout,
				Err:      errors.Join(parseErr, captureErr),
			}, chunks, nil
		}
	}

	dr := DispatchResult{
		Outcome:  ClassifyOutcome(exec.ExitCode, parseErr, nil, intent.ResolvedInputs.NonRetryableExitCodes),
		ExitCode: copyIntPtr(exec.ExitCode),
		Outputs:  outputs,
		Stdout:   exec.Stdout,
		Files:    files,
	}
	if parseErr != nil {
		dr.Err = parseErr
	}
	return dr, chunks, nil
}

func copyIntPtr(v int) *int {
	out := v
	return &out
}

// containerSpecFor builds the DTO the Backend.Create consumes from the IR.
// Slice 4.1 (Phase 4): reads Image + Resources for the Docker backend; the
// fake ignores both.
//
// Package-level pure function (not a method on *LocalDispatcher) — its
// output depends only on wf and name, never on dispatcher state, and the
// pure form is trivially unit-testable without a *LocalDispatcher instance.
// Unexported because the only caller is engine/map.go in the same package.
func containerSpecFor(wf *ir.Workflow, name string) container.ContainerSpec {
	c, ok := wf.Containers[name]
	if !ok {
		// Validator (Phase 1.4) should have caught this; defensive return.
		return container.ContainerSpec{Name: name}
	}
	spec := container.ContainerSpec{
		Name:  name,
		Image: c.Image,
	}
	if c.Resources != nil {
		spec.Resources = &container.ContainerResources{
			CPU: c.Resources.CPU,
			Mem: c.Resources.Mem,
		}
	}
	// Compose fields land in slice 4.3.
	return spec
}

// WithItemHandle returns a shallow clone of d with Handles cloned and the
// (name → h) entry overridden (or inserted). Slice 3.4: the map executor
// (engine/map.go) calls this per item to retarget body's container lookup
// to a per-item handle. Cheap — Handles is typically 1-3 entries.
//
// The returned LocalDispatcher shares Backend with d. Mutating the returned
// dispatcher's Handles after construction is safe (it's a fresh map).
func (d *LocalDispatcher) WithItemHandle(name string, h container.Handle) *LocalDispatcher {
	cloned := make(map[string]container.Handle, len(d.Handles)+1)
	for k, v := range d.Handles {
		cloned[k] = v
	}
	cloned[name] = h
	return &LocalDispatcher{
		Backend: d.Backend,
		Handles: cloned,
	}
}
