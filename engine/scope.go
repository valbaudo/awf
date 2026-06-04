package engine

import (
	"fmt"
	"sort"
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
// step.<id>.stdout, step.<id>.<field>, evaluate.<field> (slice 3.3),
// <as>.<field> (slice 3.4 — bound inside a map body via the per-item record
// in RunState.MapItems). Roots not in this list return AWF4002 unresolved.
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
	wfRef           *ir.Workflow // slice 3.4 — needed by resolveAsBinding's mapPathIndex
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
		wfRef:     wf,
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
		wfRef:           wf,
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
		// Slice 3.4: try to resolve as a map <as> binding.
		// resolveAsBinding returns AWF4002 if no enclosing map matches.
		return s.resolveAsBinding(ref)
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
	if len(ref.Segments) < 2 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "`step` requires id + field (e.g. step.foo.exit_code)"}
	}
	idSeg := ref.Segments[1]
	if err := mustIdent(idSeg, "step id"); err != nil {
		return nil, err
	}
	staticPath, ok := s.stepIndex[idSeg.Ident]
	if !ok {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "step %q not declared in workflow (referenced at %s)", idSeg.Ident, s.ctxPath)
	}
	// Map-output aggregation (Approach A): a step.<id>[.<field>...] ref whose
	// producer sits in the v1 single-map shape, evaluated from OUTSIDE that map,
	// resolves to the index-ordered compact []any of committed per-item outputs.
	// Checked BEFORE the len<3 reject so a 2-segment whole-output aggregate
	// (step.<id>) is allowed. Non-aggregate refs fall through unchanged.
	if agg, isAgg, err := s.aggregateMapOutputs(staticPath, ref); err != nil {
		return nil, err
	} else if isAgg {
		return agg, nil
	}
	if len(ref.Segments) < 3 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "`step` requires id + field (e.g. step.foo.exit_code)"}
	}
	fieldSeg := ref.Segments[2]
	if err := mustIdent(fieldSeg, "step field"); err != nil {
		return nil, err
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

// resolveAsBinding handles `<as>` refs from within a map's body. Slice 3.4 +
// spec §5.7. Walks ctxPath back-to-front looking for `map[N].item-K` segment
// pairs; for each, looks up the corresponding ir.Map node and checks
// map.As == ref.Segments[0].Ident. The INNERMOST matching map wins.
//
// Segment semantics:
//   - `<as>` alone (1 segment): the bound ItemValue (over[K]).
//   - `<as>.index`: the integer K (regardless of ItemValue type — even if
//     ItemValue is itself a map with an "index" key, the special-case wins).
//   - `<as>.<field>` (or deeper): descend into ItemValue via descendPath.
//
// Errors:
//   - No enclosing map with matching `as` → "unknown ref root" (mirrors the
//     pre-slice-3.4 behavior for any non-reserved root).
//   - MapItemRecord present but ItemValue is nil → "value not bound"
//     (defense-in-depth: runtime invariant violated; the map executor MUST
//     have populated via UpdateMapItemValue BEFORE body's templates resolve).
func (s *Scope) resolveAsBinding(ref *template.Ref) (any, error) {
	head := ref.Segments[0]
	// Build/cache the wf map-path index. For Phase 3 minimum, rebuild per
	// call — cheap (small wfs); a future slice can cache on Scope if hot.
	idx := mapPathIndex(s.wf())
	mapPath, n, ok := enclosingMapForBinding(s.ctxPath, idx, head.Ident)
	if !ok {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"unknown ref root %q (Phase 3 supports run / input / step / evaluate / <as> inside a map body)", head.Ident)
	}
	items := s.rs.LookupMapItems(mapPath)
	var itemValue any
	var found bool
	for _, mr := range items {
		if mr.N == n {
			itemValue = mr.ItemValue
			found = true
			break
		}
	}
	if !found {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"map %q item N=%d not recorded in RunState — runtime invariant violation (map executor must RecordMapItem before body templates resolve)", mapPath, n)
	}
	if itemValue == nil {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"map %q item N=%d value not bound — runtime invariant violation (executor must UpdateMapItemValue before body templates resolve)", mapPath, n)
	}
	// `<as>` alone → the bound value itself.
	//
	// MD-A: if ItemValue is a composite (map / []any), the downstream
	// template.renderScalar will reject it with a cryptic "non-renderable
	// composite" error. Detect here and emit an actionable error pointing
	// the author at the field-access form (`{{ <as>.<field> }}` or
	// `{{ <as>.index }}`). Scalars (string / number / bool) pass through
	// to renderScalar normally.
	if len(ref.Segments) == 1 {
		switch v := itemValue.(type) {
		case map[string]any:
			fields := mapKeysFor(v)
			return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
				"`{{ %s }}` resolves to a composite (object) — use field access like `{{ %s.<field> }}` or `{{ %s.index }}`; available fields: %v",
				head.Ident, head.Ident, head.Ident, fields)
		case []any:
			return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
				"`{{ %s }}` resolves to an array (len=%d) — use index access like `{{ %s.0 }}` or `{{ %s.index }}`",
				head.Ident, len(v), head.Ident, head.Ident)
		}
		return itemValue, nil
	}
	// `<as>.index` → the integer N, regardless of ItemValue type.
	if len(ref.Segments) == 2 && !ref.Segments[1].IsIndex && ref.Segments[1].Ident == "index" {
		return n, nil
	}
	// `<as>.<field>...` → descend into ItemValue.
	return descendPath(itemValue, ref.Segments[1:], head.Ident+".")
}

// mapKeysFor returns the sorted keys of m for inclusion in diagnostic
// messages. Sorted for stable error text (test-friendly).
func mapKeysFor(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// wf returns the workflow stored in this Scope. The existing scope holds wf
// only indirectly via stepIndex; slice 3.4 adds a direct reference for the
// mapPathIndex walker.
func (s *Scope) wf() *ir.Workflow {
	return s.wfRef
}

// mapPathIndex walks wf.Graph and returns a map: static-map-path → *ir.Map.
// Used by resolveAsBinding to look up the As name for a given map path.
func mapPathIndex(wf *ir.Workflow) map[string]*ir.Map {
	out := map[string]*ir.Map{}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		if m, ok := n.(*ir.Map); ok {
			out[path] = m
		}
	})
	return out
}

// enclosingMapForBinding walks ctxPath back-to-front looking for `map[N].item-K`
// segment pairs. For each, looks up the map in idx (converting the runtime
// mapPath to its static-path equivalent first — see runtimeMapPathToStatic);
// if the map's As matches the asName, returns the map's runtime path + the K
// value. INNERMOST match wins.
//
// Returns (runtimePath, n, true) on match — the path is the RUNTIME form so
// callers can pass it to RunState.LookupMapItems (which keys on runtime paths).
//
// Patterns recognized:
//   - <prefix>.map[N].item-K.<rest>        — inside a map's body at item K
//   - <prefix>.map[N].item-K              — directly at item-K (no body step yet)
//
// The walker requires segments[i] starts with "item-" AND segments[i-1] starts
// with "map[" — so a step id literally "item-3" buried elsewhere in the path
// (e.g. as a step ID inside a loop's body that's inside a map.body) doesn't
// false-positive (verified by TestEnclosingMapForBindingTable's "step-id
// item-3" case).
//
// idx is keyed by STATIC IR paths (built by mapPathIndex via ir.PathFor +
// ir.ChildPath which uses ".body" between map and inner). Runtime paths use
// ".item-K" instead of ".body" at map-body junctions. runtimeMapPathToStatic
// performs the conversion at lookup time so nested-map bindings resolve
// correctly (slice 3.4 CR-A fix; pinned by TestRuntimeMapPathToStatic +
// TestScopeResolveAsBindingNestedMaps).
func enclosingMapForBinding(ctxPath string, idx map[string]*ir.Map, asName string) (string, int, bool) {
	if ctxPath == "" {
		return "", 0, false
	}
	segments := strings.Split(ctxPath, ".")
	// Walk from end backward. For each i ≥ 1 where segments[i] starts with
	// "item-" AND segments[i-1] starts with "map[":
	//   * mapPath = strings.Join(segments[:i], ".")   (runtime form)
	//   * static  = runtimeMapPathToStatic(mapPath)    (for idx lookup)
	//   * n = parseN(segments[i] after "item-")
	//   * if idx[static].As == asName → match.
	for i := len(segments) - 1; i >= 1; i-- {
		seg := segments[i]
		if !strings.HasPrefix(seg, "item-") {
			continue
		}
		if !strings.HasPrefix(segments[i-1], "map[") {
			continue
		}
		mapPath := strings.Join(segments[:i], ".") // runtime path (with .item-K)
		// CR-A: convert to static form for idx lookup. Nested maps have
		// ctxPaths like "map[0].item-0.map[0].item-2.step"; the runtime
		// mapPath for the inner map is "map[0].item-0.map[0]", but idx
		// keys are "map[0]" and "map[0].body.map[0]" (static). The
		// converter replaces ".item-K" with ".body" where preceded by
		// "map[X]" — only at map-body junctions; loop.body.iter-K is
		// left untouched (loop static path already uses ".body.iter-K"
		// in NEITHER form — actually loop's runtime IS the static form
		// for loops, since loop.body + iter-K matches both).
		staticMapPath := runtimeMapPathToStatic(mapPath)
		m, ok := idx[staticMapPath]
		if !ok {
			// idx may not contain this path (e.g. a malformed runtime path, or a
			// map under a dead/never-built branch). Defense-in-depth: skip and
			// continue walking.
			continue
		}
		if m.As != asName {
			// This map's binding name doesn't match; keep walking outward.
			continue
		}
		nStr := strings.TrimPrefix(seg, "item-")
		n, perr := strconv.Atoi(nStr)
		if perr != nil {
			// Malformed item segment — should never happen (engine.ItemPath
			// produces valid ints); defense-in-depth: skip.
			continue
		}
		return mapPath, n, true // return RUNTIME path for LookupMapItems
	}
	return "", 0, false
}

// runtimeMapPathToStatic converts a runtime path to its static-IR-path
// equivalent by replacing ".item-K" segments with ".body" wherever the
// preceding segment starts with "map[". Used by enclosingMapForBinding to
// look up nested maps in the static-keyed mapPathIndex (CR-A fix).
//
// Examples (also pinned by TestRuntimeMapPathToStatic):
//
//	"map[0]"                              → "map[0]"               (identity)
//	"map[0].item-0.map[0]"                → "map[0].body.map[0]"   (nested)
//	"map[0].item-0.map[0].item-2"         → "map[0].body.map[0].body"
//	"map[0].item-0.loop[0].body.iter-3"   → "map[0].body.loop[0].body.iter-3"
//	    (loop's body+iter is untouched — only map.item-K gets converted)
//	"map[0].item-0.item-3"                → "map[0].body.item-3"
//	    (a step id literally "item-3" inside map body: not preceded by "map[",
//	     stays as item-3)
//
// The replacement rule (item-K preceded by map[X] → body) is safe because:
//   - Loop bodies use ".body.iter-K" both in static AND runtime forms (ir.ChildPath +
//     runtime IterPath both produce the same ".body.iter-K" suffix).
//   - Step ids matching `item-\d+` are valid per AWF1020 but, because they're
//     leaves (no preceding "map[" in the segments array), the walker skips
//     them via the same check.
func runtimeMapPathToStatic(runtimePath string) string {
	segs := strings.Split(runtimePath, ".")
	for i := 1; i < len(segs); i++ {
		if strings.HasPrefix(segs[i], "item-") && strings.HasPrefix(segs[i-1], "map[") {
			segs[i] = "body"
		}
	}
	return strings.Join(segs, ".")
}

// aggregateMapOutputs resolves step.<id>[.<field>...] when the producer is in the
// v1 single-map shape and the ref site is OUTSIDE that map: returns the index-
// ordered, compact []any of committed per-item outputs (whole output for a 2-seg
// ref, the descended field otherwise). isAgg=false → not an aggregate; caller falls
// through to normal resolution.
//
// NOTE: per-item descendPath errors are returned WITHOUT the step-context wrapper
// (they are item-specific; the validator already guarantees the field exists, so
// this path is defensive).
func (s *Scope) aggregateMapOutputs(staticPath string, ref *template.Ref) (any, bool, error) {
	mapStatic, suffix, ok := ir.SingleMapBodyShape(staticPath)
	if !ok {
		return nil, false, nil
	}
	if _, inside, err := s.instanceFromCtx(mapStatic, itemSep); err != nil {
		return nil, false, err
	} else if inside {
		return nil, false, nil // same-item ref → normal resolution
	}
	idSeg := ref.Segments[1]
	items := s.rs.LookupMapItems(mapStatic) // shallow copy — safe to sort in place
	sort.Slice(items, func(i, j int) bool { return items[i].N < items[j].N })
	out := []any{}
	for _, mr := range items {
		rp := ItemPath(mapStatic, mr.N)
		if suffix != "" {
			rp += "." + suffix
		}
		nr, ok := s.rs.LookupCompleted(rp)
		if !ok || nr.Outputs == nil {
			continue // compact: only items where the producer committed typed output
		}
		if len(ref.Segments) == 2 {
			out = append(out, nr.Outputs)
			continue
		}
		val, err := descendPath(nr.Outputs, ref.Segments[2:], "step."+idSeg.Ident+".")
		if err != nil {
			return nil, true, err
		}
		out = append(out, val)
	}
	return out, true, nil
}

// stepRuntimePath converts a step's static IR path (from StepPathIndex) to the
// runtime path that keys into RunState.Completed. It inserts the per-instance
// segment at each multiplicity boundary the static path crosses:
//
//   - loop[N].body → loop[N].body.iter-K — K is the current iter (ctxPath inside
//     the loop) or the latest completed iter (ctxPath outside): a loop ref
//     resolves to "the most recent iteration" (spec §5.2), so loops are
//     transparent from outside.
//   - gate[N] → gate[N].attempt-M — M is the attempt ctxPath sits in. A gate's
//     internal steps are referenceable ONLY from within the same attempt
//     (generate sibling, evaluate, or until); from outside there is no defined
//     "which attempt", so this errors (AWF4002). The gate's product is read via
//     evaluate.<field>, not by addressing its internal steps.
//   - map[N].body → map[N].item-K — K is the item ctxPath sits in. Map body
//     steps are referenceable ONLY from within the same item; items run
//     concurrently so there is no "most recent" — a cross-item / external ref
//     errors (AWF4002). Aggregate access is deferred per spec §11.
//
// try / parallel introduce no multiplicity, so their segments pass through and a
// step inside them resolves from anywhere, exactly like a top-level step.
//
// Nested loops (multiple loop[…] segments in one static path) remain rejected —
// the LoopIters wire format for nested loops is unspecified (slice 2.3 design
// question 3); distinct nested kinds (loop in map, map in gate, …) are supported
// because each scope's per-instance state is keyed by its runtime path.
func (s *Scope) stepRuntimePath(staticPath string) (string, error) {
	segments := strings.Split(staticPath, ".")

	loopCount := 0
	for _, seg := range segments {
		if strings.HasPrefix(seg, "loop[") {
			loopCount++
		}
	}
	if loopCount > 1 {
		return "", fmt.Errorf("nested loops not supported (LoopIters wire format for nested loops is unspecified); static path %q has %d loop segments", staticPath, loopCount)
	}

	cur := "" // runtime path built so far (no trailing separator)
	for i, seg := range segments {
		switch {
		case seg == "body" && i > 0 && strings.HasPrefix(segments[i-1], "loop["):
			// loop[N].body → loop[N].body.iter-K
			cur = appendSeg(cur, seg)
			iter, err := s.iterForLoop(cur)
			if err != nil {
				return "", err
			}
			cur = IterPath(cur, iter)
		case seg == "body" && i > 0 && strings.HasPrefix(segments[i-1], "map["):
			// map[N].body → map[N].item-K (the body segment is replaced by item-K).
			k, matched, err := s.instanceFromCtx(cur, itemSep)
			if err != nil {
				return "", err
			}
			if !matched {
				return "", fmt.Errorf("step inside map %q is only referenceable from within the same item; cross-item or aggregate access is not defined (spec §11)", cur)
			}
			cur = ItemPath(cur, k)
		default:
			cur = appendSeg(cur, seg)
			if strings.HasPrefix(seg, "gate[") {
				// gate[N] → gate[N].attempt-M (inserted before the following
				// generate/evaluate/until segment).
				m, matched, err := s.instanceFromCtx(cur, attemptSep)
				if err != nil {
					return "", err
				}
				if !matched {
					return "", fmt.Errorf("step inside gate %q is only referenceable from within the same attempt; read the gate's product via evaluate.<field>", cur)
				}
				cur = AttemptPath(cur, m)
			}
		}
	}
	return cur, nil
}

// appendSeg joins one path segment onto cur with the '.' separator (cur=="" → seg).
func appendSeg(cur, seg string) string {
	if cur == "" {
		return seg
	}
	return cur + "." + seg
}

// instanceFromCtx reads the per-instance index that ctxPath assigns to the scope
// rooted at scopePrefix, where the runtime suffix is sep+<int> (sep is iterSep /
// attemptSep / itemSep). Three outcomes:
//
//   - (n, true, nil)   — ctxPath is inside this scope instance; n is its index.
//   - (0, false, nil)  — ctxPath is not within this scope (the reference crosses
//     the boundary); the caller decides (loop falls back to latest, gate/map error).
//   - (0, true, err)   — ctxPath prefix-matches but the index isn't an integer;
//     a malformed path must error rather than silently fall back (engine invariant).
func (s *Scope) instanceFromCtx(scopePrefix, sep string) (int, bool, error) {
	full := scopePrefix + sep
	if !strings.HasPrefix(s.ctxPath, full) {
		return 0, false, nil
	}
	rest := s.ctxPath[len(full):]
	end := strings.IndexByte(rest, '.')
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, true, fmt.Errorf("malformed instance segment in ctxPath %q (after prefix %q)", s.ctxPath, full)
	}
	return n, true, nil
}

// iterForLoop returns the iteration number to use for a loop's `.body` segment.
// Same-iter rule: if ctxPath is inside `<loopBodyPath>.iter-K`, use that K.
// Otherwise: K = RunState.LoopIters[loopPath] (the latest completed iter — the
// "most recent iteration" rule that makes loops transparent from outside). Zero
// iters AND not inside the loop → error (no value to return).
func (s *Scope) iterForLoop(loopBodyPath string) (int, error) {
	if !strings.HasSuffix(loopBodyPath, ".body") {
		return 0, fmt.Errorf("internal: iterForLoop called with non-body path %q", loopBodyPath)
	}
	n, matched, err := s.instanceFromCtx(loopBodyPath, iterSep)
	if err != nil {
		return 0, err
	}
	if matched {
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
