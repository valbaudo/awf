package engine

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// Scope is the slice 2.3 adapter that satisfies template.Scope by reading from
// a RunState and a workflow IR. The interpreter (slice 2.5) constructs one per
// template evaluation (substitute a run: command, evaluate an if.cond) with the
// ctxPath set to the runtime path of the node about to be processed (the path
// MAY include iter-N segments — Scope reads them to implement the "same-iter"
// step-resolution case in spec §5.2).
//
// Phase 2 reference vocabulary: run.id, input.<field>, step.<id>.exit_code,
// step.<id>.<field>. step.<id>.stdout is deferred to slice 2.4 (returns
// AWF4099 in slice 2.3 — see the plan's Design question 2). Roots not in this
// list — evaluate.* (Phase 3 gate), <as>.* (Phase 3 map) — return AWF4002
// unresolved. The validator (slice 1.4) already catches the static cases; the
// runtime closes the loop on anything that slipped past.
//
// Nested loops are out of scope for slice 2.3 (see plan Design question 3);
// stepRuntimePath errors with a clear "nested loops not supported" message
// rather than silently computing a wrong path.
type Scope struct {
	rs        *RunState
	ctxPath   string
	stepIndex map[string]string // step id → static IR path (precomputed once at NewScope)
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

// Resolve implements template.Scope. Dispatches on the first ref segment; the
// AWF4001 size check is NOT performed here — template.resolveRefValue applies
// it uniformly after the Scope returns.
func (s *Scope) Resolve(ref *template.Ref) (any, error) {
	if len(ref.Segments) == 0 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "empty ref"}
	}
	head := ref.Segments[0]
	if head.IsIndex {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "ref must start with an identifier"}
	}
	switch head.Ident {
	case "run":
		return s.resolveRun(ref)
	case "input":
		return s.resolveInput(ref)
	case "step":
		return s.resolveStep(ref)
	default:
		return nil, &template.EvalError{
			Code: template.EvalCodeRefUnresolved,
			Msg:  fmt.Sprintf("unknown ref root %q (Phase 2 supports run / input / step)", head.Ident),
		}
	}
}

func (s *Scope) resolveRun(ref *template.Ref) (any, error) {
	if len(ref.Segments) != 2 || ref.Segments[1].IsIndex || ref.Segments[1].Ident != "id" {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "only `run.id` is defined"}
	}
	return s.rs.RunID, nil
}

func (s *Scope) resolveInput(ref *template.Ref) (any, error) {
	if len(ref.Segments) < 2 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "`input` requires a field selector"}
	}
	var cur any = s.rs.Input
	for i := 1; i < len(ref.Segments); i++ {
		seg := ref.Segments[i]
		next, err := descend(cur, seg)
		if err != nil {
			return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "input." + segPath(ref.Segments[1:i+1]) + ": " + err.Error()}
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
	if idSeg.IsIndex {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step id must be an identifier"}
	}
	fieldSeg := ref.Segments[2]
	if fieldSeg.IsIndex {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step field must be an identifier"}
	}
	// Defer stdout to slice 2.4 BEFORE looking up the step — gives a clearer
	// "not yet" message even if the step itself isn't committed yet.
	if fieldSeg.Ident == "stdout" {
		return nil, &template.EvalError{
			Code: template.EvalCodeDeferred,
			Msg:  fmt.Sprintf("step.%s.stdout resolution lands in slice 2.4 (Phase 2 plan Design question 2)", idSeg.Ident),
		}
	}
	staticPath, ok := s.stepIndex[idSeg.Ident]
	if !ok {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: fmt.Sprintf("step %q not declared in workflow", idSeg.Ident)}
	}
	runtimePath, err := s.stepRuntimePath(staticPath)
	if err != nil {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: err.Error()}
	}
	nr, ok := s.rs.Completed[runtimePath]
	if !ok {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: fmt.Sprintf("step %q not yet committed (runtime path %q)", idSeg.Ident, runtimePath)}
	}
	switch fieldSeg.Ident {
	case "exit_code":
		if nr.ExitCode == nil {
			return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step has no exit_code (agent or signal step?)"}
		}
		return *nr.ExitCode, nil
	default:
		// Typed output field — look up in nr.Outputs, then descend further if more segments.
		if nr.Outputs == nil {
			return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: fmt.Sprintf("field %q: step has no typed outputs", fieldSeg.Ident)}
		}
		var cur any = nr.Outputs
		for i := 2; i < len(ref.Segments); i++ {
			seg := ref.Segments[i]
			next, err := descend(cur, seg)
			if err != nil {
				return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "step." + idSeg.Ident + "." + segPath(ref.Segments[2:i+1]) + ": " + err.Error()}
			}
			cur = next
		}
		return cur, nil
	}
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
			fmt.Fprintf(&b, ".iter-%d", iter)
		}
	}
	return b.String(), nil
}

// iterForLoop returns the iteration number to use for a loop's `.body` segment.
// Same-iter rule: if ctxPath starts with `<loopBodyPath>.iter-`, use that K.
// Otherwise: K = RunState.LoopIters[loopPath] (the latest completed iter). Zero
// iters AND not inside the loop → error (no value to return).
func (s *Scope) iterForLoop(loopBodyPath string) (int, error) {
	prefix := loopBodyPath + ".iter-"
	if strings.HasPrefix(s.ctxPath, prefix) {
		rest := s.ctxPath[len(prefix):]
		end := strings.IndexByte(rest, '.')
		if end < 0 {
			end = len(rest)
		}
		if n, err := strconv.Atoi(rest[:end]); err == nil {
			return n, nil
		}
	}
	loopPath := strings.TrimSuffix(loopBodyPath, ".body")
	iter, ok := s.rs.LoopIters[loopPath]
	if !ok || iter == 0 {
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

// StepPathIndex walks wf.Graph and returns a map from step id → static IR path.
// Phase 2 supports CodeStep / AgentStep / SignalStep / If / Loop kinds; the
// remaining control kinds (Try / Parallel / Gate / Map / Skip) are skipped —
// the interpreter (slice 2.5) errors on them at runtime in Phase 2, so any
// step buried inside them is unreachable. Phase 3+ extends this walker.
//
// Duplicate step ids are caught by the validator (AWF1004, slice 1.4) — this
// function trusts the input was validated and last-write-wins on duplicates.
//
// The returned strings are exactly what ir.PathFor / ir.ChildPath produce; see
// ir/path_test.go for the canonical examples ("triage" / "loop[1].body.echo" /
// "loop[1].body.if[1].then.deep_step").
func StepPathIndex(wf *ir.Workflow) map[string]string {
	out := map[string]string{}
	walkNodes(wf.Graph, "", out)
	return out
}

func walkNodes(list ir.NodeList, parent string, out map[string]string) {
	for i, n := range list {
		switch v := n.(type) {
		case *ir.CodeStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.AgentStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.SignalStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.If:
			walkNodes(v.Then, ir.ChildPath(parent, "if", i, "then"), out)
			walkNodes(v.Else, ir.ChildPath(parent, "if", i, "else"), out)
		case *ir.Loop:
			walkNodes(v.Body, ir.ChildPath(parent, "loop", i, "body"), out)
			// Try / Parallel / Gate / Map / Skip — Phase 2 doesn't execute them; skip.
			// Slice 2.5 will fail at the interpreter for any workflow that uses them.
			// Phase 3+ extends this walker to recurse into them.
		}
	}
}
