package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// ErrNodeNotImplementedInPhase3 is the sentinel the interpreter returns for any
// node kind Phase 3 doesn't execute. After slice 3.2: Try, Skip, and Parallel
// ship; SignalStep / Gate / Map remain unimplemented (each shipping in its own
// Phase 3 slice). After all five Phase 3 slices ship, only AgentStep remains —
// slice 3.5 will rename to ErrNodeNotImplemented (phase-agnostic).
//
// The per-slice phase-string in notImpl identifies the landing slice per kind.
// Distinct from engine.ErrUnsupportedKind (the dispatcher's per-step sentinel)
// — the two answer different questions, and conflating them would let a future
// Phase 4+ caller mis-route an AgentStep through dispatcher plumbing.
//
// Wrap with kind + path for diagnostic clarity:
//
//	fmt.Errorf("%w: %s at path %q", ErrNodeNotImplementedInPhase3, kindName, path)
var ErrNodeNotImplementedInPhase3 = errors.New("engine: node kind not implemented in Phase 3")

// Run is the top-level interpreter entry point. Walks def.Workflow.Graph
// recursively; for each node, computes its runtime path; consults runstate to
// skip committed nodes (the resume invariant — slice 2.6 exercises the
// non-empty-Completed case); otherwise dispatches per-kind. Halts on the first
// error or non-ok outcome that escapes any try/catch enclosing it (slice 3.1
// added try/catch; future slices add gate/parallel/map — each defines its own
// absorption rule per Phase 3 spec §5).
//
// Returns:
//   - (OutcomeOK, nil)                              — every node committed ok,
//     OR a Skip unwound to the
//     workflow root (spec §5.6)
//   - (OutcomeRetryableFailure | OutcomePermanentFailure, <step's err>)
//     — a step terminated; the
//     run.finished event records
//     the same outcome
//   - ("", <internal-error>)                        — interpreter / CLI bug
//     (unknown container,
//     ErrNodeNotImplementedInPhase3,
//     log/blobs failure). The CLI
//     distinguishes this from
//     step failures via the empty
//     Outcome (slice 2.5 DQ4).
//
// tap, if non-nil, receives the live-tap output: one "[step.id] <chunk>" per
// IOChunk produced by each step. nil disables the tap entirely (per Phase 2
// design — Phase 6 will replace this with the obs subsystem).
//
// dispatcher's Handles map MUST be populated for every container the workflow
// declares; engine.Run is NOT responsible for Backend.Create / Destroy (the
// CLI owns lifecycle, slice 2.5 DQ3). An unknown-container reference inside
// a step is an internal error.
func Run(
	ctx context.Context,
	def *ir.LoadedDefinition,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	if def == nil || def.Workflow == nil {
		return "", fmt.Errorf("engine.Run: nil workflow")
	}
	if runstate == nil {
		return "", fmt.Errorf("engine.Run: nil runstate")
	}
	oc, err := interpNodes(ctx, def.Workflow.Graph, "", def.Workflow, runstate, dispatcher, log, blobs, clk, tap)
	// SkipUnwind reaching Run = workflow-root unwind target (no enclosing
	// loop/try/parallel/gate/map caught it). Per Phase 3 spec §5.6: "Cleanly
	// terminates the nearest enclosing scope ... or (if none) the run, as ok."
	var su *SkipUnwind
	if errors.As(err, &su) {
		// Append node.skipped{path: "", reason} for trace (Phase 6 obs).
		if appendErr := appendNodeSkipped(log, "", su.Reason); appendErr != nil {
			return "", appendErr
		}
		return OutcomeOK, nil
	}
	return oc, err
}

// interpNodes recursively walks a NodeList in order. Each node's path is
// computed via ir.PathFor — the single source of truth for journal keys
// (CLAUDE.md "node addressing is one pure function" invariant; runtime
// suffixes — iter-N for Loop body — are added inside the loop handler, not
// here).
func interpNodes(
	ctx context.Context,
	nodes ir.NodeList,
	parent string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	for i, n := range nodes {
		oc, err := interpNode(ctx, n, i, parent, wf, runstate, dispatcher, log, blobs, clk, tap)
		if oc != OutcomeOK || err != nil {
			return oc, err
		}
	}
	return OutcomeOK, nil
}

// interpNode handles one node. Kind-dispatch lives here so each recursive call
// site shares the same per-kind switch.
func interpNode(
	ctx context.Context,
	n ir.Node,
	idx int,
	parent string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	switch v := n.(type) {
	case *ir.CodeStep:
		return runCodeStep(ctx, v, ir.PathFor(parent, "", v.ID, idx), wf, runstate, dispatcher, log, blobs, clk, tap)
	case *ir.If:
		return runIf(ctx, v, ir.PathFor(parent, "if", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap)
	case *ir.Loop:
		return runLoop(ctx, v, ir.PathFor(parent, "loop", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap)
	case *ir.AgentStep:
		return notImpl("agent", ir.PathFor(parent, "", v.ID, idx), "Phase 5")
	case *ir.SignalStep:
		return notImpl("signal", ir.PathFor(parent, "", v.ID, idx), "Phase 3 slice 3.5")
	case *ir.Try:
		return runTry(ctx, v, ir.PathFor(parent, "try", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap)
	case *ir.Parallel:
		return runParallel(ctx, v, ir.PathFor(parent, "parallel", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap)
	case *ir.Gate:
		return notImpl("gate", ir.PathFor(parent, "gate", "", idx), "Phase 3 slice 3.3")
	case *ir.Map:
		return notImpl("map", ir.PathFor(parent, "map", "", idx), "Phase 3 slice 3.4")
	case *ir.Skip:
		return runSkip(v)
	default:
		return "", fmt.Errorf("engine: unknown node type %T at parent %q index %d (validator should have caught)", n, parent, idx)
	}
}

// notImpl builds the standard ErrNodeNotImplementedInPhase3 wrap for a node
// kind whose handler isn't implemented yet. Centralizes the error format
// so the deferred cases in interpNode share one shape — adding/removing a
// kind in a later slice is a one-line edit at the call site.
func notImpl(kind, path, phase string) (Outcome, error) {
	return "", fmt.Errorf("%w: %s at path %q (%s)", ErrNodeNotImplementedInPhase3, kind, path, phase)
}

// runCodeStep is the CodeStep handler — composes substitution, retry, dispatch,
// classify, commit, AND failure-event emission. Each of those primitives is
// slice 2.4's; this function is the connecting tissue.
//
// Flow:
//
//  1. Resume-check — if path ∈ runstate.Completed, skip silently.
//  2. Build the template Scope rooted at this step's path; substitute cs.Run
//     and cs.IdempotencyKey. Template errors → node.failed{permanent_failure}
//     (slice 2.5 DQ7).
//  3. Merge retry policy: default + cs.Retry. Unknown backoff → internal error
//     halt (slice 2.4 DQ8).
//  4. Build NodeIntent + ResolvedInputs.
//  5. RunWithRetry — handles attempts + backoff + retry.attempt events.
//  6. On OK: drain the live-tap channel, then Commit. Update Completed[path].
//  7. On non-ok: drain the live-tap channel, then append node.failed{outcome,
//     error}. Return (outcome, err) — propagate up.
//  8. On internal error from RunWithRetry (unknown container, etc.): no
//     node.failed event (the step never got to fail mechanically — this is
//     the CLI's bug, not the step's). Return ("", err).
func runCodeStep(
	ctx context.Context,
	cs *ir.CodeStep,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	if _, done := runstate.LookupCompleted(path); done {
		return OutcomeOK, nil
	}

	scope := NewScope(runstate, wf, path)
	command, err := template.Substitute(cs.Run, scope)
	if err != nil {
		return failStep(log, path, OutcomePermanentFailure, err)
	}
	idemKey := ""
	if cs.IdempotencyKey != nil {
		idemKey, err = template.Substitute(string(*cs.IdempotencyKey), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, err)
		}
	}

	policy, err := retry.Merge(retry.Default, cs.Retry)
	if err != nil {
		return "", fmt.Errorf("engine.Run: build retry policy at path %q: %w", path, err)
	}

	resolved := ResolvedInputs{
		Command:               command,
		Env:                   map[string]string{},
		OutputFiles:           cs.OutputFiles,
		OutputSchema:          cs.OutputSchema,
		NonRetryableExitCodes: policy.NonRetryableExitCodes,
	}
	if cs.Timeout != nil {
		resolved.Timeout = time.Duration(*cs.Timeout)
	}

	intent := NodeIntent{
		Path:           path,
		Node:           cs,
		ResolvedInputs: resolved,
		IdempotencyKey: idemKey,
	}

	dr, chunks, runErr := RunWithRetry(ctx, dispatcher, intent, policy, clk, log)
	// Drain the live-tap channel (single consumer — fine for Phase 2's
	// pre-closed fake channels; Phase 4's Docker streaming will require
	// this be moved to a goroutine).
	drainTap(chunks, cs.ID, tap)

	if runErr != nil {
		// dr.Outcome == "" means RunWithRetry never successfully dispatched (the
		// dispatcher itself returned a non-nil error on the first attempt — unknown
		// container, ErrUnsupportedKind, transport-classifier bug). No step outcome
		// was produced; treat as an internal error, no node.failed event.
		// Note: RunWithRetry can also return non-nil errors with a non-empty Outcome
		// (ctx cancellation mid-backoff, retry.attempt log failure). Those fall
		// through to failStep below — the last attempt's outcome is the right
		// classification, and node.failed records the underlying cause.
		if dr.Outcome == "" {
			return "", fmt.Errorf("engine.Run: dispatch at path %q: %w", path, runErr)
		}
		// Step outcome class: runErr is the underlying cause.
		return failStep(log, path, dr.Outcome, runErr)
	}

	if dr.Outcome != OutcomeOK {
		// Defensive — RunWithRetry should always return non-nil err on non-ok.
		return failStep(log, path, dr.Outcome, errors.New("step did not commit (no underlying error reported)"))
	}

	nr, err := Commit(log, blobs, path, dr)
	if err != nil {
		return "", fmt.Errorf("engine.Run: commit at path %q: %w", path, err)
	}
	runstate.RecordCompleted(path, nr)
	return OutcomeOK, nil
}

// runIf is the If handler. Spec §5.1 + design spec §F: evaluate cond, append
// branch.taken, recurse into the chosen branch. Resume-safe: if
// runstate.Branches[path] is already set, take the recorded branch without
// re-evaluating (the §8 "committed steps are replayed, not recomputed"
// invariant applies to the branch decision — re-evaluating could choose the
// OTHER branch if cond depends on a step output that's now in Completed).
//
// The child branch path is `path + "." + which` — a literal join (path is
// already "if[N]" via ir.PathFor at the interpNode dispatch). ir.ChildPath
// isn't used here because we already have the if's path computed; ChildPath
// is the parent→child computation for static analysis.
func runIf(
	ctx context.Context,
	n *ir.If,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	which, recorded := runstate.LookupBranch(path)
	if !recorded {
		scope := NewScope(runstate, wf, path)
		// Template-error class (parse: AWF4005; eval: AWF4001/4002/4003/4004) —
		// DQ7: permanent_failure for the if NODE. The error is the author's bug.
		cond, err := template.EvalBoolString(string(n.Cond), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, err)
		}
		if cond {
			which = "then"
		} else {
			which = "else"
		}
		data, err := json.Marshal(BranchTakenData{Which: which})
		if err != nil {
			return "", fmt.Errorf("engine.Run: marshal branch.taken at path %q: %w", path, err)
		}
		if err := log.Append(state.Event{
			Type: EventBranchTaken,
			Path: path,
			Data: data,
		}); err != nil {
			return "", fmt.Errorf("engine.Run: append branch.taken at path %q: %w", path, err)
		}
		// branch.taken is observational — no Log.Sync (rides the next fsync per
		// spec §8 cost lever). A branch.taken lost to a torn tail means resume
		// re-evaluates the cond, which is correct first-run-equivalent behavior.
		runstate.RecordBranch(path, which)
	}

	switch which {
	case "then":
		return interpNodes(ctx, n.Then, path+".then", wf, runstate, dispatcher, log, blobs, clk, tap)
	case "else":
		// nil/empty Else is a no-op per spec §5.1.
		if len(n.Else) == 0 {
			return OutcomeOK, nil
		}
		return interpNodes(ctx, n.Else, path+".else", wf, runstate, dispatcher, log, blobs, clk, tap)
	default:
		// Validator should reject; this is corruption / bug defense.
		return "", fmt.Errorf("engine.Run: unknown branch %q at path %q", which, path)
	}
}

// runLoop is the Loop handler. Spec §5.2 + design spec §F + slice 2.5 DQ8:
// do-while iteration. Body runs; loop.iter{n} appends ONLY after body succeeds
// (DQ8 invariant — body failure means iter never completed, no loop.iter
// event). until is tested AFTER each iter (so it can reference what the body
// just produced); max_iters bounds the loop. Resume-safe: K starts at
// runstate.LoopIters[path] + 1.
//
// The validator (ir/validate_structural.go) enforces "at least one of until /
// max_iters" per spec §5.2. The runtime defends against validator regression
// by erroring loud if both are nil (slice 2.5 R7) — silent exit on iter 1
// would mask the validator bug.
func runLoop(
	ctx context.Context,
	n *ir.Loop,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	// Defense-in-depth FIRST: avoid entering the loop with a definition that
	// validation should have rejected.
	if n.Until == nil && n.MaxIters == nil {
		return "", fmt.Errorf("engine: loop at path %q has neither until nor max_iters — validator regression (AWF §5.2 requires one); please report", path)
	}

	startK := runstate.LookupLoopIters(path) + 1
	for k := startK; ; k++ {
		bodyParent := IterPath(path+".body", k)
		// 1. Walk the body for iter K.
		oc, err := interpNodes(ctx, n.Body, bodyParent, wf, runstate, dispatcher, log, blobs, clk, tap)
		// SkipUnwind from body = iteration end target. Append node.skipped for
		// trace, then continue to the loop.iter append below — the iter
		// completed (via skip) and loop.iter records that for resume.
		var su *SkipUnwind
		skipped := errors.As(err, &su)
		if skipped {
			if appendErr := appendNodeSkipped(log, bodyParent, su.Reason); appendErr != nil {
				return "", appendErr
			}
			// Clear oc/err to fall through to the loop.iter + until/max_iters
			// path — the iter is "complete" from the loop's perspective.
			oc, err = OutcomeOK, nil
		}
		if oc != OutcomeOK || err != nil {
			// Body failed — DQ8: do NOT emit loop.iter for this iter.
			return oc, err
		}
		// 2. Body completed. Append loop.iter{n: k}.
		data, mErr := json.Marshal(LoopIterData{N: k})
		if mErr != nil {
			return "", fmt.Errorf("engine.Run: marshal loop.iter at path %q iter %d: %w", path, k, mErr)
		}
		if err := log.Append(state.Event{
			Type: EventLoopIter,
			Path: path,
			Data: data,
		}); err != nil {
			return "", fmt.Errorf("engine.Run: append loop.iter at path %q iter %d: %w", path, k, err)
		}
		// loop.iter is observational — no Log.Sync (rides next fsync).
		runstate.RecordLoopIter(path, k)

		// 3. Evaluate until (if set). True → exit. Scope rooted at bodyParent so
		//    step.<id>.<field> refs resolve to THIS iter's outputs (spec §5.2
		//    "most recent iteration"). Template errors → permanent_failure (DQ7).
		if n.Until != nil {
			scope := NewScope(runstate, wf, bodyParent)
			done, evalErr := template.EvalBoolString(string(*n.Until), scope)
			if evalErr != nil {
				return failStep(log, path, OutcomePermanentFailure, evalErr)
			}
			if done {
				return OutcomeOK, nil
			}
		}

		// 4. Check max_iters. K >= max → exit.
		if n.MaxIters != nil && k >= *n.MaxIters {
			return OutcomeOK, nil
		}
	}
}

// failStep is the shared failure-emission helper. Appends a node.failed event
// AND returns the (outcome, err) tuple the caller propagates. The event is
// fsync'd via Log.Sync so a torn-tail post-halt still observes the failure
// record — node.failed IS the terminal record for the step; losing it means
// resume can't see what happened.
func failStep(log state.Log, path string, outcome Outcome, cause error) (Outcome, error) {
	errStr := ""
	if cause != nil {
		errStr = cause.Error()
	}
	data, err := json.Marshal(NodeFailedData{
		Outcome: string(outcome),
		Error:   errStr,
	})
	if err != nil {
		return "", fmt.Errorf("engine.Run: marshal node.failed at path %q: %w", path, err)
	}
	if err := log.Append(state.Event{
		Type: EventNodeFailed,
		Path: path,
		Data: data,
	}); err != nil {
		return "", fmt.Errorf("engine.Run: append node.failed at path %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return "", fmt.Errorf("engine.Run: sync log after node.failed at path %q: %w", path, err)
	}
	return outcome, cause
}

// drainTap consumes the per-attempt IOChunk channel from RunWithRetry's last
// attempt and writes each chunk to tap (prefixed with "[<stepID>] "). nil tap
// or nil channel are both no-ops. Blocks until the channel is closed —
// Phase 2's pre-closed fake channels make this finite.
//
// On tap-write error we KEEP DRAINING the channel (with tap disabled). A
// streaming Phase 4 backend whose writer goroutine fills the channel buffer
// would deadlock if the consumer stops reading.
//
// We track the "tap is still good" state in a LOCAL variable `w`, never
// mutating the `tap` parameter.
func drainTap(chunks <-chan container.IOChunk, stepID string, tap io.Writer) {
	if chunks == nil {
		return
	}
	w := tap
	for c := range chunks {
		if w == nil {
			continue // still drain; don't write
		}
		if _, err := fmt.Fprintf(w, "[%s] %s", stepID, c.Data); err != nil {
			w = nil
		}
	}
}
