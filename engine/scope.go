package engine

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// Scope is the slice 2.3 adapter that satisfies template.Scope by reading from
// a RunState and a workflow IR. The interpreter constructs one per template
// evaluation (substitute a run: command, evaluate an if.cond, evaluate a
// gate.until) with the ctxPath set to the runtime path of the node about to
// be processed.
//
// Phase 3 reference vocabulary: run.id, input.<field>, step.<id>.exit_code,
// step.<id>.stdout, step.<id>.<field>, evaluate.<field> (slice 3.3). Roots
// not in this list — <as>.* (Phase 3 map) — return AWF4002 unresolved.
//
// verdictOverride: optional. When non-nil, evaluate.* resolves against it
// directly INSTEAD of consulting RunState.GateAttempts. The gate executor
// (engine/gate.go) sets this when evaluating gate.until — the just-produced
// verdict isn't yet in GateAttempts (the gate.attempt event hasn't committed),
// so the override carries the verdict through.
//
// Nested loops are out of scope for slice 2.3 (see plan Design question 3);
// stepRuntimePath errors with a clear "nested loops not supported" message
// rather than silently computing a wrong path.
type Scope struct {
	rs              *RunState
	ctxPath         string
	stepIndex       map[string]string // step id → static IR path (computed at NewScope; one IR walk per scope)
	verdictOverride map[string]any
}

// NewScope wires the inputs into a Scope. ctxPath is the runtime path of the
// node about to be evaluated — the static IR path with each loop-body segment
// suffixed by ".iter-N" for the current iteration. See the plan's "ctxPath
// contract" table for what to pass at each evaluation site.
func NewScope(rs *RunState, wf *ir.Workflow, ctxPath string) *Scope {
	return &Scope{
		rs:        rs,
		ctxPath:   ctxPath,
		stepIndex: StepPathIndex(wf),
	}
}

// NewScopeWithVerdict constructs a Scope with verdictOverride set. Used by the
// gate executor (engine/gate.go) when evaluating gate.until: the just-produced
// verdict is bound to evaluate.* before the gate.attempt event commits.
//
// gatePath is the static gate path (e.g. "gate[0]"); ctxPath is synthesized as
// "<gatePath>.attempt-<N>.until" — but callers pass the full synthesized path,
// not gatePath alone (this constructor doesn't synthesize).
func NewScopeWithVerdict(rs *RunState, wf *ir.Workflow, ctxPath string, verdict map[string]any) *Scope {
	return &Scope{
		rs:              rs,
		ctxPath:         ctxPath,
		stepIndex:       StepPathIndex(wf),
		verdictOverride: verdict,
	}
}

// Resolve implements template.Scope. Dispatches on the first ref segment; the
// AWF4001 size check is NOT performed here — template.resolveRefValue applies
// it uniformly after the Scope returns.
//
// Returned composite values (map[string]any, []any) alias the underlying
// RunState — callers MUST NOT mutate them (see RunState.NodeResult's
// READ-ONLY caveat).
func (s *Scope) Resolve(ref *template.Ref) (any, error) {
	if len(ref.Segments) == 0 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "empty ref"}
	}
	head := ref.Segments[0]
	if err := mustIdent(head, "ref root"); err != nil {
		return nil, err
	}
	switch head.Ident {
	case "run":
		return s.resolveRun(ref)
	case "input":
		return s.resolveInput(ref)
	case "step":
		return s.resolveStep(ref)
	case "evaluate":
		return s.resolveEvaluate(ref)
	default:
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "unknown ref root %q (Phase 3 supports run / input / step / evaluate)", head.Ident)
	}
}

func (s *Scope) resolveRun(ref *template.Ref) (any, error) {
	if len(ref.Segments) != 2 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "only `run.id` is defined"}
	}
	if err := mustIdent(ref.Segments[1], "run.<field>"); err != nil {
		return nil, err
	}
	if ref.Segments[1].Ident != "id" {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "only `run.id` is defined"}
	}
	return s.rs.RunID, nil
}

func (s *Scope) resolveInput(ref *template.Ref) (any, error) {
	if len(ref.Segments) < 2 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "`input` requires a field selector"}
	}
	return descendPath(s.rs.Input, ref.Segments[1:], "input.")
}

// mustIdent returns an AWF4002 EvalError if seg is an index segment; nil otherwise.
// Used for ref positions where only identifiers are valid (e.g. ref roots, step
// ids, step field names).
func mustIdent(seg template.Segment, role string) error {
	if seg.IsIndex {
		return &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: role + " must be an identifier (not an index)"}
	}
	return nil
}

// descendPath walks segs from start, calling descend at each step. On error,
// wraps with an AWF4002 EvalError whose Msg starts with prefix +
// segPath(consumed) where consumed is the segments walked so far (1-based).
func descendPath(start any, segs []template.Segment, prefix string) (any, error) {
	cur := start
	for i, seg := range segs {
		next, err := descend(cur, seg)
		if err != nil {
			return nil, &template.EvalError{
				Code: template.EvalCodeRefUnresolved,
				Msg:  prefix + segPath(segs[:i+1]) + ": " + err.Error(),
			}
		}
		cur = next
	}
	return cur, nil
}

func (s *Scope) resolveStep(ref *template.Ref) (any, error) {
	if len(ref.Segments) < 3 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "`step` requires id + field (e.g. step.foo.exit_code)"}
	}
	idSeg := ref.Segments[1]
	if err := mustIdent(idSeg, "step id"); err != nil {
		return nil, err
	}
	fieldSeg := ref.Segments[2]
	if err := mustIdent(fieldSeg, "step field"); err != nil {
		return nil, err
	}
	staticPath, ok := s.stepIndex[idSeg.Ident]
	if !ok {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "step %q not declared in workflow", idSeg.Ident)
	}
	runtimePath, err := s.stepRuntimePath(staticPath)
	if err != nil {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: err.Error()}
	}
	nr, ok := s.rs.LookupCompleted(runtimePath)
	if !ok {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "step %q not yet committed (runtime path %q)", idSeg.Ident, runtimePath)
	}
	switch fieldSeg.Ident {
	case "exit_code":
		if nr.ExitCode == nil {
			return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step has no exit_code (agent or signal step?)"}
		}
		return *nr.ExitCode, nil
	case "stdout":
		// nr.Stdout is materialized by Fold from NodeCompletedData.StdoutRef
		// (slice 2.4 atomic extension). Return string so EvalBool comparisons
		// (`step.x.stdout == "ok\n"`) work without coercion. nil Stdout → "".
		if len(ref.Segments) != 3 {
			return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step.<id>.stdout takes no further segments"}
		}
		return string(nr.Stdout), nil
	default:
		// Typed output field — look up in nr.Outputs, then descend further if more segments.
		if nr.Outputs == nil {
			return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "field %q: step has no typed outputs", fieldSeg.Ident)
		}
		return descendPath(nr.Outputs, ref.Segments[2:], "step."+idSeg.Ident+".")
	}
}

// resolveEvaluate handles `evaluate.<field>` refs. Per Phase 3 slice 3.3
// design §D + decision 9:
//
//  1. If verdictOverride is set (gate.until evaluation against just-produced
//     verdict that isn't yet in GateAttempts), descend into it.
//  2. Else, identify the enclosing gate via enclosingGateForEvaluate(ctxPath).
//     If none (ctxPath isn't under a gate's generate / until), error.
//  3. Read RunState.GateAttempts[gatePath]; if empty (attempt 1, no prior
//     verdict), resolve to empty string "" — safe substitution for the
//     typical {{ evaluate.feedback }} template per design §D.
//  4. Else, descend into the LATEST attempt's verdict.
func (s *Scope) resolveEvaluate(ref *template.Ref) (any, error) {
	if len(ref.Segments) < 2 {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "`evaluate` requires a field selector (e.g. evaluate.feedback)")
	}
	if s.verdictOverride != nil {
		return descendPath(s.verdictOverride, ref.Segments[1:], "evaluate.")
	}
	gatePath, ok := enclosingGateForEvaluate(s.ctxPath)
	if !ok {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"`evaluate.<field>` is only meaningful inside a gate's generate or until (ctxPath=%q)", s.ctxPath)
	}
	attempts := s.rs.LookupGateAttempts(gatePath)
	if len(attempts) == 0 {
		// Attempt 1: no prior verdict. Return "" so {{ evaluate.feedback }}
		// safely substitutes to empty (design §D — feedback safe on attempt 1).
		// Arithmetic comparisons on attempt 1 will type-mismatch (AWF4003); the
		// author should guard with conditionals if mixing typed reads with
		// attempt-1 evaluations.
		return "", nil
	}
	latest := attempts[len(attempts)-1]
	if latest.Verdict == nil {
		return "", nil
	}
	return descendPath(latest.Verdict, ref.Segments[1:], "evaluate.")
}

// stepRuntimePath converts a step's static IR path (from StepPathIndex) to the
// runtime path that keys into RunState.Completed. Each `loop[N].body` segment
// pair gets suffixed with `.iter-K` where K is the current iter (if ctxPath is
// inside that loop) or the latest completed iter (if ctxPath is outside).
//
// Nested loops (multiple loop[…] segments in one static path) are explicitly
// rejected — slice 2.3 doesn't support them (see plan Design question 3).
func (s *Scope) stepRuntimePath(staticPath string) (string, error) {
	segments := strings.Split(staticPath, ".")

	// Guard: nested loops are out of scope for slice 2.3.
	loopCount := 0
	for _, seg := range segments {
		if strings.HasPrefix(seg, "loop[") {
			loopCount++
		}
	}
	if loopCount > 1 {
		return "", fmt.Errorf("nested loops not supported in slice 2.3 (LoopIters wire format for nested loops is unspecified in Phase 2 design); static path %q has %d loop segments", staticPath, loopCount)
	}

	var b strings.Builder
	for i, seg := range segments {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(seg)
		if seg == "body" && i > 0 && strings.HasPrefix(segments[i-1], "loop[") {
			loopPath := strings.Join(segments[:i+1], ".") // up to and including ".body"
			iter, err := s.iterForLoop(loopPath)
			if err != nil {
				return "", err
			}
			b.WriteString(iterSep)
			b.WriteString(strconv.Itoa(iter))
		}
	}
	return b.String(), nil
}

// iterForLoop returns the iteration number to use for a loop's `.body` segment.
// Same-iter rule: if ctxPath starts with `<loopBodyPath>.iter-`, use that K.
// Otherwise: K = RunState.LoopIters[loopPath] (the latest completed iter). Zero
// iters AND not inside the loop → error (no value to return).
func (s *Scope) iterForLoop(loopBodyPath string) (int, error) {
	if !strings.HasSuffix(loopBodyPath, ".body") {
		return 0, fmt.Errorf("internal: iterForLoop called with non-body path %q", loopBodyPath)
	}
	prefix := iterPrefix(loopBodyPath)
	if strings.HasPrefix(s.ctxPath, prefix) {
		rest := s.ctxPath[len(prefix):]
		end := strings.IndexByte(rest, '.')
		if end < 0 {
			end = len(rest)
		}
		n, err := strconv.Atoi(rest[:end])
		if err != nil {
			return 0, fmt.Errorf("malformed iter segment in ctxPath %q (after prefix %q)", s.ctxPath, prefix)
		}
		return n, nil
	}
	loopPath := strings.TrimSuffix(loopBodyPath, ".body")
	iter := s.rs.LookupLoopIters(loopPath)
	if iter == 0 {
		return 0, fmt.Errorf("loop %q has no completed iterations", loopPath)
	}
	return iter, nil
}

// descend takes one segment-step into a value: map[ident] for object descents,
// []any[idx] for index descents. Returns a typed error explaining the mismatch.
func descend(cur any, seg template.Segment) (any, error) {
	if seg.IsIndex {
		arr, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("index %d into non-array (%T)", seg.Index, cur)
		}
		if seg.Index >= len(arr) {
			return nil, fmt.Errorf("index %d out of range [0, %d)", seg.Index, len(arr))
		}
		return arr[seg.Index], nil
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("field %q in non-object (%T)", seg.Ident, cur)
	}
	v, ok := m[seg.Ident]
	if !ok {
		return nil, fmt.Errorf("field %q not present", seg.Ident)
	}
	return v, nil
}

func segPath(segs []template.Segment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.IsIndex {
			parts = append(parts, strconv.Itoa(s.Index))
			continue
		}
		parts = append(parts, s.Ident)
	}
	return strings.Join(parts, ".")
}
