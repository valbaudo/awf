package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// runGate is the Gate handler (Phase 3 spec §5.5 + design §D). Implements the
// generate → evaluate → repair state machine, with the crash≠verdict
// invariant pinned in design §D step 5: a gate.attempt event commits ONLY
// after a complete attempt (generate ok + evaluate ok + until evaluated). Any
// mechanical failure of a generate or evaluate step propagates BEFORE the
// gate.attempt event is written — no attempt consumed.
//
// State machine:
//
//	startN := len(runstate.GateAttempts[gatePath]) + 1
//	for n := startN; n <= g.MaxAttempts; n++ {
//	  attemptPath := AttemptPath(gatePath, n)
//
//	  1. Run generate. interpNodes(attemptPath + ".generate", g.Generate).
//	     Any non-nil non-skip err → return (oc, err); NO commit.
//	     SkipUnwind → terminal-ok WHOLE-GATE (design §D + slice 3.3 design Q3);
//	     append node.skipped{gatePath, reason}; return (OutcomeOK, nil).
//
//	  2. Run evaluate. interpNodes(attemptPath + ".evaluate", g.Evaluate).
//	     Same crash / skip handling.
//
//	  3. Compute verdict: look up the LAST evaluator step's NodeResult from
//	     runstate.Completed (it was committed during step 2). The last node's
//	     OutputsRef + Outputs is the verdict.
//
//	  4. Evaluate g.Until via NewScopeWithVerdict(verdict). True → attempt_passed;
//	     false → attempt_rejected. Template error → permanent_failure (the
//	     author wrote an invalid until expression; matches runIf / runLoop DQ7).
//
//	  5. Commit gate.attempt event: marshal GateAttemptData{n, attempt_outcome,
//	     verdict_ref}, Log.Append, Log.Sync. Any error here → return ("", err)
//	     — internal error, NOT a gate rejection.
//
//	  6. RecordGateAttempt — in-memory mirror.
//
//	  7. If attempt_passed: return (OutcomeOK, nil).
//	     Else if n == g.MaxAttempts: return (OutcomeRejected, gateRejectedError).
//	     Else: continue to attempt n+1.
//	}
//
// The single-place commit at step 5 is the load-bearing crash≠verdict invariant.
func runGate(
	ctx context.Context,
	g *ir.Gate,
	gatePath string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	return runGateWithContext(ctx, g, gatePath, interpreterContext{
		wf: wf, runstate: runstate, dispatcher: dispatcher, log: log, blobs: blobs, clk: clk, tap: tap, broker: broker,
	})
}

func runGateWithContext(
	ctx context.Context,
	g *ir.Gate,
	gatePath string,
	ictx interpreterContext,
) (Outcome, error) {
	if len(g.Generate) == 0 {
		// Validator (AWF1013) rejects; defense-in-depth.
		return "", fmt.Errorf("engine.runGate: gate %q has empty generate (AWF1013 should have caught)", gatePath)
	}
	if len(g.Evaluate) == 0 {
		return "", fmt.Errorf("engine.runGate: gate %q has empty evaluate", gatePath)
	}
	if g.MaxAttempts < 1 {
		return "", fmt.Errorf("engine.runGate: gate %q has MaxAttempts=%d, want ≥ 1", gatePath, g.MaxAttempts)
	}

	startN := len(ictx.runstate.LookupGateAttempts(gatePath)) + 1

	for n := startN; n <= g.MaxAttempts; n++ {
		attemptPath := AttemptPath(gatePath, n) // "gate[0].attempt-1"
		generatePath := attemptPath + ".generate"
		evaluatePath := attemptPath + ".evaluate"

		// 1. Run generate.
		genOC, genErr := interpNodes(ctx, g.Generate, generatePath, ictx)
		var su *SkipUnwind
		if errors.As(genErr, &su) {
			// Skip ends WHOLE gate as ok (design §D + slice 3.3 design Q3).
			if appErr := appendNodeSkipped(ictx.log, gatePath, su.Reason); appErr != nil {
				return "", appErr
			}
			return OutcomeOK, nil
		}
		if genErr != nil || genOC != OutcomeOK {
			// Crash≠verdict: propagate BEFORE the gate.attempt event.
			return genOC, genErr
		}

		// 2. Run evaluate.
		evalOC, evalErr := interpNodes(ctx, g.Evaluate, evaluatePath, ictx)
		if errors.As(evalErr, &su) {
			if appErr := appendNodeSkipped(ictx.log, gatePath, su.Reason); appErr != nil {
				return "", appErr
			}
			return OutcomeOK, nil
		}
		if evalErr != nil || evalOC != OutcomeOK {
			return evalOC, evalErr
		}

		// 3. Look up the last evaluator step's NodeResult → verdict.
		lastStepPath, lerr := lastEvaluatorPath(g, evaluatePath)
		if lerr != nil {
			return "", fmt.Errorf("engine.runGate: %w", lerr)
		}
		nr, ok := ictx.runstate.LookupCompleted(lastStepPath)
		if !ok {
			return "", fmt.Errorf("engine.runGate: last evaluator step %q not in Completed (slice 3.3 invariant violated)", lastStepPath)
		}
		verdict := nr.Outputs // typed outputs — validated against output_schema by Phase 1.4 AWF1014

		// 4. Evaluate g.Until against the just-produced verdict.
		untilCtxPath := attemptPath + ".until"
		untilScope := ictx.scopeWithVerdict(untilCtxPath, verdict)
		passed, untilErr := template.EvalBoolString(string(g.Until), untilScope)
		if untilErr != nil {
			// Author's bug — invalid until expression. Same classification as
			// runIf / runLoop's DQ7 (permanent_failure for the gate node).
			return failStep(ictx.log, gatePath, OutcomePermanentFailure, untilErr)
		}
		attemptOutcome := AttemptRejected
		if passed {
			attemptOutcome = AttemptPassed
		}

		// 5. Commit gate.attempt event.
		data, mErr := json.Marshal(GateAttemptData{
			N:              n,
			AttemptOutcome: attemptOutcome,
			VerdictRef:     nr.OutputsRef,
		})
		if mErr != nil {
			return "", fmt.Errorf("engine.runGate: marshal gate.attempt at %q: %w", gatePath, mErr)
		}
		if err := ictx.log.Append(state.Event{
			Type: EventGateAttempt,
			Path: gatePath,
			Data: data,
		}); err != nil {
			return "", fmt.Errorf("engine.runGate: append gate.attempt at %q: %w", gatePath, err)
		}
		if err := ictx.log.Sync(); err != nil {
			return "", fmt.Errorf("engine.runGate: sync after gate.attempt at %q: %w", gatePath, err)
		}

		// 6. RecordGateAttempt — in-memory mirror.
		ictx.runstate.RecordGateAttempt(gatePath, AttemptResult{
			N:              n,
			AttemptOutcome: attemptOutcome,
			Verdict:        verdict,
		})

		// 7. Pass / max-reject / continue.
		if passed {
			return OutcomeOK, nil
		}
		if n == g.MaxAttempts {
			return OutcomeRejected, fmt.Errorf("engine.runGate: gate %q rejected after %d attempts", gatePath, n)
		}
		// Continue to attempt n+1; GateAttempts now has the verdict-1 entry for
		// the next generate's evaluate.* feedback.
	}

	// Unreachable: the loop returns on every iteration; the postcondition is
	// the same as the n==MaxAttempts case above.
	return OutcomeRejected, fmt.Errorf("engine.runGate: gate %q rejected (loop fall-through, should not happen)", gatePath)
}

// lastEvaluatorPath returns the runtime path of the last node in g.Evaluate
// under the given evaluatePath prefix. The last node MUST be a step kind with
// output_schema, or (jury-panel Task 2) a map whose reduce: produces a typed
// verdict (AWF1014 guarantees this for valid workflows). For resume-
// correctness, the path must be addressable via the same scheme the step
// handlers use (ir.PathFor for steps, ir.ChildPath for control nodes).
func lastEvaluatorPath(g *ir.Gate, evaluatePath string) (string, error) {
	if len(g.Evaluate) == 0 {
		return "", fmt.Errorf("empty evaluate")
	}
	idx := len(g.Evaluate) - 1
	last := g.Evaluate[idx]
	switch v := last.(type) {
	case *ir.CodeStep:
		return ir.PathFor(evaluatePath, "", v.ID, idx), nil
	case *ir.AgentStep:
		return ir.PathFor(evaluatePath, "", v.ID, idx), nil
	case *ir.SignalStep:
		return ir.PathFor(evaluatePath, "", v.ID, idx), nil
	case *ir.Map:
		// A map terminates evaluate: (AWF1014 relaxed, jury-panel Task 2) — its
		// reduce commits the verdict at the map's OWN path (bare map[i], no id
		// segment — ir/walk.go's map addressing). The aggregate output_schema
		// (quorum synthetic or run reducer's) is what `until` / evaluate.<field>
		// reads. Mirrors ir/validate_structural.go's nodeHasOutputSchema *Map arm;
		// the two must agree on exactly which maps are valid evaluate terminals.
		return ir.PathFor(evaluatePath, "map", "", idx), nil
	default:
		// Validator AWF1014 ensures the last node is a step, or a map with a
		// typed reduce. Defense-in-depth: surface the unexpected kind clearly.
		return "", fmt.Errorf("last evaluator node is %T (must be a step kind, or a map with a typed reduce, per AWF1014)", last)
	}
}
