package engine

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// Compile-time check: engine.Scope satisfies template.Scope.
var _ template.Scope = (*Scope)(nil)

// fakeShaImage is a syntactically-valid digest-pinned image ref for test
// workflow fixtures. The 64-'a' hex string is technically a valid sha256.
// The validator (slice 1.4) would accept it at definition time.
const fakeShaImage = "oci://example@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const testRunID = "run-X"

// minimalWorkflow returns a small workflow shape used by the tests below. The
// shape: index-0 = triage (CodeStep, top-level), index-1 = loop containing
// body=[echo, inner_if], inner_if has then=[deep_step]. Covers the Phase 2
// path grammar that exercises loop-iter substitution AND nested control flow.
//
// Static paths (per ir.PathFor / ir.ChildPath — see ir/path_test.go):
//
//	triage    → "triage"                              (top-level step by id)
//	echo      → "loop[1].body.echo"                   (step inside loop body)
//	deep_step → "loop[1].body.if[1].then.deep_step"   (step inside if-then inside loop)
func minimalWorkflow() *ir.Workflow {
	one := 1
	return &ir.Workflow{
		ID: "test", Version: 1,
		Containers: map[string]ir.Container{
			"lab": {Image: fakeShaImage},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "triage", Container: "lab", Run: "echo triage"}, // index 0
			&ir.Loop{ // index 1
				MaxIters: &one,
				Body: ir.NodeList{
					&ir.CodeStep{ID: "echo", Container: "lab", Run: "echo body"}, // body index 0
					&ir.If{ // body index 1
						Cond: "input.flag",
						Then: ir.NodeList{
							&ir.CodeStep{ID: "deep_step", Container: "lab", Run: "echo deep"},
						},
					},
				},
			},
		},
	}
}

func TestStepPathIndex(t *testing.T) {
	wf := minimalWorkflow()
	idx := StepPathIndex(wf)
	want := map[string]string{
		"triage":    "triage",
		"echo":      "loop[1].body.echo",
		"deep_step": "loop[1].body.if[1].then.deep_step",
	}
	for id, wantPath := range want {
		got, ok := idx[id]
		if !ok {
			t.Errorf("idx[%q] missing", id)
			continue
		}
		if got != wantPath {
			t.Errorf("idx[%q] = %q, want %q", id, got, wantPath)
		}
	}
	if len(idx) != len(want) {
		t.Errorf("idx has %d entries, want %d (full: %v)", len(idx), len(want), idx)
	}
}

func TestScopeResolveRunAndInput(t *testing.T) {
	rs := &RunState{
		RunID: testRunID,
		Input: map[string]any{"cve_id": "CVE-2024-9999", "flag": true},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")

	ref := mustParseRef(t, "run.id")
	v, err := sc.Resolve(ref)
	if err != nil || v != testRunID {
		t.Errorf("run.id: v=%v err=%v", v, err)
	}

	ref = mustParseRef(t, "input.cve_id")
	v, err = sc.Resolve(ref)
	if err != nil || v != "CVE-2024-9999" {
		t.Errorf("input.cve_id: v=%v err=%v", v, err)
	}

	ref = mustParseRef(t, "input.flag")
	v, err = sc.Resolve(ref)
	if err != nil || v != true {
		t.Errorf("input.flag: v=%v err=%v", v, err)
	}

	// Missing input field → AWF4002.
	ref = mustParseRef(t, "input.missing")
	_, err = sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("input.missing: err=%v, want AWF4002", err)
	}
}

func TestScopeResolveInputIndexSegment(t *testing.T) {
	// Index segments (`input.list.0.field`) — for inputs containing arrays.
	rs := &RunState{
		Input: map[string]any{
			"cves": []any{
				map[string]any{"id": "CVE-1"},
				map[string]any{"id": "CVE-2"},
			},
		},
	}
	sc := NewScope(rs, minimalWorkflow(), "triage")
	ref := mustParseRef(t, "input.cves.0.id")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "CVE-1" {
		t.Errorf("v = %v, want %q", v, "CVE-1")
	}

	// Out-of-range index → AWF4002.
	ref = mustParseRef(t, "input.cves.5.id")
	_, err = sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("out-of-range: err=%v, want AWF4002", err)
	}
}

func TestScopeResolveStepFromOutsideLoop(t *testing.T) {
	// Resolving step.echo from OUTSIDE the loop: spec §5.2 says it resolves to
	// the latest completed iter's result (RunState.LoopIters["loop[1]"] = 2).
	rs := &RunState{
		RunID:     testRunID,
		LoopIters: map[string]int{"loop[1]": 2},
		Completed: map[string]NodeResult{
			"loop[1].body.iter-1.echo": {Outcome: OutcomeOK, ExitCode: intp(0), Outputs: map[string]any{"n": 1.0}},
			"loop[1].body.iter-2.echo": {Outcome: OutcomeOK, ExitCode: intp(0), Outputs: map[string]any{"n": 2.0}},
		},
	}
	wf := minimalWorkflow()
	// ctxPath = a hypothetical node AFTER the loop. The IR doesn't have one in
	// this fixture, but ctxPath is just a string the scope reads to detect "are
	// we inside loop[1]?" — outside, the LoopIters fallback applies.
	sc := NewScope(rs, wf, "after_loop")

	// step.echo.n → iter-2's typed output (latest completed).
	ref := mustParseRef(t, "step.echo.n")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("step.echo.n: %v", err)
	}
	if v != 2.0 {
		t.Errorf("step.echo.n = %v, want 2.0", v)
	}

	// step.echo.exit_code → iter-2's exit code.
	ref = mustParseRef(t, "step.echo.exit_code")
	v, err = sc.Resolve(ref)
	if err != nil {
		t.Fatalf("step.echo.exit_code: %v", err)
	}
	if got, ok := v.(int); !ok || got != 0 {
		t.Errorf("step.echo.exit_code = %v (%T), want int 0", v, v)
	}
}

func TestScopeResolveStepFromInsideSameLoopIter(t *testing.T) {
	// Resolving step.echo from inside the same loop body's iter-3 — must
	// resolve to iter-3's result (the current iteration), NOT iter-2 (the
	// "latest completed" which would be wrong because iter-3 is the current).
	rs := &RunState{
		RunID:     testRunID,
		LoopIters: map[string]int{"loop[1]": 2}, // 2 iters completed, iter-3 is running
		Completed: map[string]NodeResult{
			"loop[1].body.iter-2.echo": {Outcome: OutcomeOK, ExitCode: intp(0), Outputs: map[string]any{"n": 2.0}},
			"loop[1].body.iter-3.echo": {Outcome: OutcomeOK, ExitCode: intp(0), Outputs: map[string]any{"n": 3.0}}, // committed earlier in iter-3
		},
	}
	wf := minimalWorkflow()
	// ctxPath = the deep_step's runtime path in iter-3 (deep_step is inside
	// if-then inside the loop body — that's the kind of node that needs to
	// resolve a sibling step.echo).
	sc := NewScope(rs, wf, "loop[1].body.iter-3.if[1].then.deep_step")
	ref := mustParseRef(t, "step.echo.n")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("step.echo.n: %v", err)
	}
	if v != 3.0 {
		t.Errorf("step.echo.n = %v, want 3.0 (same-iter rule)", v)
	}
}

func TestScopeResolveStepOutsideAnyLoop(t *testing.T) {
	// Resolving a top-level step's ref: static path = runtime path = bare id;
	// no iter substitution.
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: intp(0), Outputs: map[string]any{"web_exploitable": true}},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "loop[1]") // ctxPath = the loop node itself (e.g. evaluating loop.until)
	ref := mustParseRef(t, "step.triage.web_exploitable")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("step.triage.web_exploitable: %v", err)
	}
	if v != true {
		t.Errorf("v = %v, want true", v)
	}
}

func TestScopeResolveUnknownStepIDIsAWF4002(t *testing.T) {
	rs := &RunState{RunID: testRunID}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")
	ref := mustParseRef(t, "step.ghost.field")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002", err)
	}
}

func TestResolveStepUndeclaredErrorNamesRefSite(t *testing.T) {
	rs := NewRunState("run-x", "digest-x", nil)
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{
		&ir.CodeStep{ID: "a", Container: "c", Run: "echo {{ step.nope.x }}"},
	}}
	s := NewScope(rs, wf, "a.run")
	_, err := s.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "nope"}, {Ident: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "a.run") {
		t.Fatalf("error should name the ref site (ctxPath); got %v", err)
	}
}

// controlKindsWorkflow returns a workflow with one step buried in each Phase 3
// control kind (try.do/catch/finally, parallel child, gate.generate/evaluate,
// map.body), so the StepPathIndex walker's recursion into every kind is pinned.
func controlKindsWorkflow() *ir.Workflow {
	step := func(id string) *ir.CodeStep { return &ir.CodeStep{ID: id, Container: "lab", Run: "echo " + id} }
	return &ir.Workflow{
		ID: "test", Version: 1,
		Containers: map[string]ir.Container{"lab": {Image: fakeShaImage}},
		Graph: ir.NodeList{
			&ir.Try{ // index 0
				Do:      ir.NodeList{step("s_do")},
				Catch:   ir.NodeList{step("s_catch")},
				Finally: ir.NodeList{step("s_fin")},
			},
			&ir.Parallel{ // index 1
				Children: ir.NodeList{step("s_par")},
			},
			&ir.Gate{ // index 2
				Generate: ir.NodeList{step("s_gen")},
				Evaluate: ir.NodeList{step("s_eval")},
			},
			&ir.Map{ // index 3
				Body: ir.NodeList{step("s_map")},
			},
		},
	}
}

func TestStepPathIndexControlKinds(t *testing.T) {
	idx := StepPathIndex(controlKindsWorkflow())
	want := map[string]string{
		"s_do":    "try[0].do.s_do",
		"s_catch": "try[0].catch.s_catch",
		"s_fin":   "try[0].finally.s_fin",
		"s_par":   "parallel[1].s_par",
		"s_gen":   "gate[2].generate.s_gen",
		"s_eval":  "gate[2].evaluate.s_eval",
		"s_map":   "map[3].body.s_map",
	}
	for id, wantPath := range want {
		got, ok := idx[id]
		if !ok {
			t.Errorf("step %q missing from index", id)
			continue
		}
		if got != wantPath {
			t.Errorf("step %q: path = %q, want %q", id, got, wantPath)
		}
	}
}

// resolveStepField is a tiny helper: build a Scope at ctxPath over wf+rs and
// resolve `step.<id>.<field>`, returning the value or error.
func resolveStepField(t *testing.T, rs *RunState, wf *ir.Workflow, ctxPath, id, field string) (any, error) {
	t.Helper()
	sc := NewScope(rs, wf, ctxPath)
	return sc.Resolve(mustParseRef(t, "step."+id+"."+field))
}

func TestScopeResolveStepInTryIsTransparent(t *testing.T) {
	wf := controlKindsWorkflow()
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"try[0].do.s_do": {Outcome: OutcomeOK, Outputs: map[string]any{"out": "ok"}},
		},
	}
	// A try step runs once; static path == runtime path. It must resolve both
	// from a sibling inside the try AND from outside it (try is transparent).
	for _, ctxPath := range []string{"try[0].do.s_catch", "after_try"} {
		got, err := resolveStepField(t, rs, wf, ctxPath, "s_do", "out")
		if err != nil {
			t.Errorf("ctxPath %q: unexpected error: %v", ctxPath, err)
			continue
		}
		if got != "ok" {
			t.Errorf("ctxPath %q: got %v, want \"ok\"", ctxPath, got)
		}
	}
}

func TestScopeResolveStepInMapSameItem(t *testing.T) {
	wf := controlKindsWorkflow()
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"map[3].item-2.s_map": {Outcome: OutcomeOK, Outputs: map[string]any{"out": "v2"}},
		},
	}
	// A reference from within item-2's body resolves to item-2's instance.
	got, err := resolveStepField(t, rs, wf, "map[3].item-2.consumer", "s_map", "out")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "v2" {
		t.Errorf("got %v, want \"v2\"", got)
	}
}

func TestScopeResolveStepInMapFromOutsideAggregates(t *testing.T) {
	// Map-output aggregation (Approach A, design §1.4): a step.<id> ref to a
	// producer inside a single map, evaluated from OUTSIDE that map, resolves to
	// the index-ordered compact []any of committed per-item outputs — it no
	// longer errors (the pre-aggregation contract was "cross-item undefined →
	// AWF4002"; this feature overturns it).
	wf := controlKindsWorkflow()
	rs := NewRunState(testRunID, testDigest, nil)
	rs.RecordMapItem("map[3]", MapItemRecord{N: 2, ItemValue: "x", Status: ItemPassed})
	rs.RecordCompleted("map[3].item-2.s_map", NodeResult{Outcome: OutcomeOK, Outputs: map[string]any{"out": "v2"}})

	// 3-seg field aggregate: step.s_map.out → ["v2"].
	got, err := resolveStepField(t, rs, wf, "after_map", "s_map", "out")
	if err != nil {
		t.Fatalf("field aggregate: unexpected error: %v", err)
	}
	arr, ok := got.([]any)
	if !ok || len(arr) != 1 || arr[0] != "v2" {
		t.Errorf("field aggregate = %#v, want []any{\"v2\"}", got)
	}

	// 2-seg whole-output aggregate: step.s_map → [{"out":"v2"}].
	sc := NewScope(rs, wf, "after_map")
	whole, err := sc.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "s_map"}}})
	if err != nil {
		t.Fatalf("whole aggregate: unexpected error: %v", err)
	}
	warr, ok := whole.([]any)
	if !ok || len(warr) != 1 {
		t.Fatalf("whole aggregate = %#v, want one element", whole)
	}
	if m, ok := warr[0].(map[string]any); !ok || m["out"] != "v2" {
		t.Errorf("whole aggregate[0] = %#v, want {\"out\":\"v2\"}", warr[0])
	}
}

func TestScopeResolveStepInGateSameAttempt(t *testing.T) {
	wf := controlKindsWorkflow()
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"gate[2].attempt-1.generate.s_gen": {Outcome: OutcomeOK, Outputs: map[string]any{"out": "g1"}},
		},
	}
	// A reference from within attempt-1 (generate sibling, evaluate, or until)
	// resolves to attempt-1's generate instance.
	// The "gate[2].attempt-1.evaluate.judge" case is the runtime counterpart of the
	// README's path-passing pattern (evaluate reads a same-attempt generate step).
	// Intentional — see awf-workflow(5) gate independence note.
	for _, ctxPath := range []string{
		"gate[2].attempt-1.generate.other",
		"gate[2].attempt-1.evaluate.judge",
		"gate[2].attempt-1.until",
	} {
		got, err := resolveStepField(t, rs, wf, ctxPath, "s_gen", "out")
		if err != nil {
			t.Errorf("ctxPath %q: unexpected error: %v", ctxPath, err)
			continue
		}
		if got != "g1" {
			t.Errorf("ctxPath %q: got %v, want \"g1\"", ctxPath, got)
		}
	}
}

func TestScopeResolveStepInGateFromOutsideIsAWF4002(t *testing.T) {
	wf := controlKindsWorkflow()
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"gate[2].attempt-1.generate.s_gen": {Outcome: OutcomeOK, Outputs: map[string]any{"out": "g1"}},
		},
	}
	_, err := resolveStepField(t, rs, wf, "after_gate", "s_gen", "out")
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002", err)
	}
}

func TestScopeResolveStepInMapInsideGate(t *testing.T) {
	// Nested distinct kinds: a step inside a map body that is itself inside a
	// gate's generate. Both per-instance segments (gate attempt-M, map item-K)
	// must be inserted, reading each index from ctxPath. This pins the spec's
	// "distinct nested kinds are supported" claim.
	wf := &ir.Workflow{
		ID: "test", Version: 1,
		Containers: map[string]ir.Container{"lab": {Image: fakeShaImage}},
		Graph: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{
					&ir.Map{Body: ir.NodeList{
						&ir.CodeStep{ID: "fetch", Container: "lab", Run: "echo fetch"},
					}},
				},
				Evaluate: ir.NodeList{&ir.CodeStep{ID: "judge", Container: "lab", Run: "true"}},
			},
		},
	}
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"gate[0].attempt-2.generate.map[0].item-3.fetch": {Outcome: OutcomeOK, Outputs: map[string]any{"out": "deep"}},
		},
	}
	// A sibling reference from within attempt-2's item-3.
	got, err := resolveStepField(t, rs, wf, "gate[0].attempt-2.generate.map[0].item-3.consumer", "fetch", "out")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "deep" {
		t.Errorf("got %v, want \"deep\"", got)
	}
}

func TestScopeResolveStepInLoopWithNoIterIsAWF4002(t *testing.T) {
	// A ref to a step inside a loop, where the loop has zero completed iters
	// AND ctxPath is outside the loop: there is no value to return.
	rs := &RunState{RunID: testRunID, LoopIters: map[string]int{}}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "after_loop")
	ref := mustParseRef(t, "step.echo.n")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002", err)
	}
}

func TestScopeResolveUnknownRefRootIsAWF4002(t *testing.T) {
	rs := &RunState{RunID: testRunID}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")
	// `evaluate.*` is Phase 3 (gate). `<as>.*` is Phase 3 (map). Either reaching
	// the engine.Scope in Phase 2 must AWF4002 (the validator should catch them
	// earlier, but the runtime closes the loop).
	for _, src := range []string{"evaluate.verified", "ghost.x"} {
		ref := mustParseRef(t, src)
		_, err := sc.Resolve(ref)
		var ee *template.EvalError
		if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
			t.Errorf("%s: err = %v, want AWF4002", src, err)
		}
	}
}

func TestScopeResolveStepStdout(t *testing.T) {
	exit := 0
	rs := &RunState{
		RunID: "run-X",
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: &exit, Stdout: []byte("hello world\n")},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "loop[1]")
	ref := mustParseRef(t, "step.triage.stdout")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := v.(string); !ok || got != "hello world\n" {
		t.Errorf("v = %v (%T), want string %q", v, v, "hello world\n")
	}
}

func TestScopeResolveStepStdoutEmpty(t *testing.T) {
	// A step that produced no stdout — Stdout is nil. Resolution returns "",
	// NOT an AWF4002 error (the step IS committed; stdout just happens to be
	// empty, which is a valid value).
	exit := 0
	rs := &RunState{
		RunID: "run-X",
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: &exit, Stdout: nil},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "loop[1]")
	ref := mustParseRef(t, "step.triage.stdout")
	v, err := sc.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := v.(string); !ok || got != "" {
		t.Errorf("v = %v (%T), want string %q", v, v, "")
	}
}

func TestScopeResolveStepStdoutOversizeIsAWF4001(t *testing.T) {
	huge := bytes.Repeat([]byte{'a'}, template.MaxInlineBytes+1)
	exit := 0
	rs := &RunState{
		RunID: "run-X",
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: &exit, Stdout: huge},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "loop[1]")
	_, err := template.Substitute("{{ step.triage.stdout }}", sc)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}

func TestScopeResolveStepWithMalformedCtxPathErrors(t *testing.T) {
	// Engine-invariant check: if ctxPath prefix-matches the loop but the iter
	// segment isn't an integer, we should error rather than silently fall back
	// to LoopIters (which would conflate "outside the loop" with "inside
	// a broken path").
	rs := &RunState{
		RunID:     testRunID,
		LoopIters: map[string]int{"loop[1]": 1},
		Completed: map[string]NodeResult{
			"loop[1].body.iter-1.echo": {Outcome: OutcomeOK, Outputs: map[string]any{"n": 1.0}},
		},
	}
	wf := minimalWorkflow()
	// Malformed: "iter-XYZ" instead of "iter-N".
	sc := NewScope(rs, wf, "loop[1].body.iter-XYZ.if[1].then.deep_step")
	ref := mustParseRef(t, "step.echo.n")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002", err)
	}
	if !strings.Contains(strings.ToLower(ee.Msg), "malformed") {
		t.Errorf("err.Msg = %q, want mention of malformed", ee.Msg)
	}
}

func TestScopeResolveStepInNestedLoopIsRejected(t *testing.T) {
	// Nested loops are out of scope for slice 2.3 (Design question 3 in the
	// plan) — LoopIters wire-format keying for nested loops needs a Phase 2.5/3
	// design pass. The runtime must fail clearly rather than silently
	// computing a wrong path.
	one := 1
	wf := &ir.Workflow{
		ID: "test", Version: 1,
		Containers: map[string]ir.Container{"lab": {Image: fakeShaImage}},
		Graph: ir.NodeList{
			&ir.Loop{
				MaxIters: &one,
				Body: ir.NodeList{
					&ir.Loop{
						MaxIters: &one,
						Body: ir.NodeList{
							&ir.CodeStep{ID: "inner", Container: "lab", Run: "echo"},
						},
					},
				},
			},
		},
	}
	rs := &RunState{RunID: testRunID}
	sc := NewScope(rs, wf, "loop[0].body.iter-1.loop[0].body.iter-1.inner")
	ref := mustParseRef(t, "step.inner.exit_code")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *template.EvalError: %v", err, err)
	}
	if !strings.Contains(strings.ToLower(ee.Msg), "nested") {
		t.Errorf("err.Msg = %q, want mention of nested loops (got code %q)", ee.Msg, ee.Code)
	}
}

func TestScopeInputOversizeIsAWF4001(t *testing.T) {
	// Verify the end-to-end AWF4001 oversize check with the real engine scope
	// in the loop — Substitute reads a too-large input value via the scope's
	// Resolve, and the template package's size check fires.
	huge := strings.Repeat("a", template.MaxInlineBytes+1)
	rs := &RunState{
		RunID: testRunID,
		Input: map[string]any{"payload": huge},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")
	_, err := template.Substitute("{{ input.payload }}", sc)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}

func TestScopeWithEvalBool(t *testing.T) {
	// Integration: engine.Scope flowing through template.EvalBool (the if.cond /
	// loop.until path). All other EvalBool tests use the test-only mapScope;
	// this pins that the real engine.Scope works with the boolean evaluator too.
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"triage": {
				Outcome:  OutcomeOK,
				ExitCode: intp(0),
				Outputs:  map[string]any{"verified": true},
			},
		},
	}
	sc := NewScope(rs, minimalWorkflow(), "after_triage")
	e, err := template.ParseExpr("step.triage.verified && step.triage.exit_code == 0")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	got, err := template.EvalBool(e, sc)
	if err != nil {
		t.Fatalf("EvalBool: %v", err)
	}
	if !got {
		t.Errorf("got %v, want true", got)
	}
}

func TestScopeResolveEvaluateInsideGenerate(t *testing.T) {
	// Simulates: a code step inside gate[0].attempt-2.generate references
	// {{ evaluate.feedback }}. RunState.GateAttempts has one prior attempt
	// (n=1) with feedback "missing X". resolveEvaluate returns that.
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{
		N: 1, AttemptOutcome: AttemptRejected,
		Verdict: map[string]any{"feedback": "missing X", "verified": false},
	})

	scope := NewScope(rs, wf, "gate[0].attempt-2.generate.step1")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "feedback"},
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "missing X" {
		t.Errorf("got %v, want \"missing X\"", got)
	}
}

func TestScopeResolveEvaluateInsideUntil(t *testing.T) {
	// gate.until evaluates against the just-produced verdict via verdictOverride.
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	scope := NewScopeWithVerdict(rs, wf, "gate[0].attempt-1.until",
		map[string]any{"verified": true, "detections": 5})

	verified, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "verified"},
	}})
	if err != nil {
		t.Fatalf("Resolve verified: %v", err)
	}
	if verified != true {
		t.Errorf("verified = %v, want true", verified)
	}
	detections, _ := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "detections"},
	}})
	if detections != 5 {
		t.Errorf("detections = %v (%T), want 5", detections, detections)
	}
}

func TestScopeResolveEvaluateOutsideGate(t *testing.T) {
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	scope := NewScope(rs, wf, "step1")
	_, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "feedback"},
	}})
	if err == nil {
		t.Fatal("Resolve evaluate.feedback outside gate: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Errorf("err = %v, want mention of \"gate\" (clarity for the author)", err)
	}
}

func TestScopeResolveEvaluateAttempt1Empty(t *testing.T) {
	// Design §D: on attempt 1, evaluate.* resolves to empty.
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	scope := NewScope(rs, wf, "gate[0].attempt-1.generate.step1")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "feedback"},
	}})
	if err != nil {
		t.Fatalf("Resolve attempt-1: %v", err)
	}
	if got != "" {
		t.Errorf("got %v, want \"\" (empty on attempt 1)", got)
	}
}

func TestScopeResolveEvaluateNestedGates(t *testing.T) {
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{
		N: 1, AttemptOutcome: AttemptRejected,
		Verdict: map[string]any{"feedback": "outer-feedback"},
	})
	rs.RecordGateAttempt("gate[0].attempt-1.generate.gate[2]", AttemptResult{
		N: 1, AttemptOutcome: AttemptRejected,
		Verdict: map[string]any{"feedback": "inner-feedback"},
	})

	scope := NewScope(rs, wf, "gate[0].attempt-1.generate.gate[2].attempt-2.generate.step1")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "feedback"},
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "inner-feedback" {
		t.Errorf("nested: got %v, want \"inner-feedback\" (innermost gate's verdict)", got)
	}
}

func TestScopeResolveEvaluateInsideEvaluateRejected(t *testing.T) {
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 1, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"feedback": "X"}})

	scope := NewScope(rs, wf, "gate[0].attempt-2.evaluate.eval_step")
	_, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "evaluate"}, {Ident: "feedback"},
	}})
	if err == nil {
		t.Fatal("evaluate.* in gate.evaluate ctxPath: err = nil, want non-nil")
	}
}

func TestEnclosingGateForEvaluateTable(t *testing.T) {
	cases := []struct {
		ctxPath string
		want    string
		wantOk  bool
	}{
		{"", "", false},
		{"step1", "", false},
		{"gate[0]", "", false},
		{"gate[0].attempt-1", "", false},
		{"gate[0].attempt-1.evaluate.x", "", false},
		{"gate[0].attempt-1.generate.step1", "gate[0]", true},
		{"gate[0].attempt-12.until", "gate[0]", true},
		{"gate[0].attempt-1.generate.gate[2].attempt-2.generate.step1", "gate[0].attempt-1.generate.gate[2]", true},
		{"gate[0].attempt-1.generate.gate[2].attempt-2.evaluate.x", "gate[0]", true},
		{"gate[0].attempt-1.generate.loop[0].body.iter-3.step1", "gate[0]", true},
	}
	for _, c := range cases {
		t.Run(c.ctxPath, func(t *testing.T) {
			got, ok := enclosingGateForEvaluate(c.ctxPath)
			if got != c.want || ok != c.wantOk {
				t.Errorf("enclosingGateForEvaluate(%q) = (%q, %v); want (%q, %v)", c.ctxPath, got, ok, c.want, c.wantOk)
			}
		})
	}
}

func TestScopeResolveArtifactPath(t *testing.T) {
	// SP1: ResolveArtifactPath(id, containerPath) → the committed CAS blob ref
	// from NodeResult.Files[containerPath] (PATH-keyed). Walk-free: the caller
	// has already mapped name→path via ir.OutputFilesByStepID.
	rs := &RunState{
		RunID: testRunID,
		Completed: map[string]NodeResult{
			"triage": {
				Outcome: OutcomeOK,
				Files:   map[string]string{"/out/r.md": "blobref-123"},
			},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "after")

	cas, err := sc.ResolveArtifactPath("triage", "/out/r.md")
	if err != nil {
		t.Fatalf("ResolveArtifactPath(triage, /out/r.md): %v", err)
	}
	if cas != "blobref-123" {
		t.Errorf("cas = %q, want %q", cas, "blobref-123")
	}

	// Undeclared step → AWF4002.
	_, err = sc.ResolveArtifactPath("nope", "/out/r.md")
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("undeclared step: err=%v, want AWF4002", err)
	}

	// Declared but not yet committed → AWF4002.
	_, err = sc.ResolveArtifactPath("echo", "/out/r.md")
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("not committed: err=%v, want AWF4002", err)
	}

	// Committed but no artifact at the path → AWF4002.
	_, err = sc.ResolveArtifactPath("triage", "/out/missing.md")
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("missing path: err=%v, want AWF4002", err)
	}
}

func mustParseRef(t *testing.T, src string) *template.Ref {
	t.Helper()
	r, err := template.ParseRef(src)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", src, err)
	}
	return r
}

// intp is a tiny test helper for taking the address of an int literal —
// removes the boilerplate `exit := 0; ExitCode: &exit` pattern when
// constructing NodeResult fixtures.
func intp(n int) *int { return &n }

func TestScopeResolveConcurrentWithCommit(t *testing.T) {
	// Phase 3 slice 3.2 — inside a parallel branch, one goroutine calls
	// runCodeStep (mutates RunState.Completed via RecordCompleted) while
	// another evaluates a template through Scope.Resolve (reads
	// RunState.Completed via LookupCompleted). Without the mutex, these
	// race; Go panics on concurrent map read+write. Run under -race.
	wf := &ir.Workflow{
		ID:      "x",
		Version: 1,
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "producer", Run: "echo", OutputSchema: &ir.JSONSchema{}},
		},
	}
	rs := NewRunState("run-x", "digest", nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rs.RecordCompleted(fmt.Sprintf("c-%d", i), NodeResult{Outcome: OutcomeOK})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			scope := NewScope(rs, wf, "")
			ref := &template.Ref{Segments: []template.Segment{
				{Ident: "step"}, {Ident: "producer"}, {Ident: "exit_code"},
			}}
			_, _ = scope.Resolve(ref) // err is allowed (unresolved); panic is the test failure
		}
	}()
	wg.Wait()
}

func TestScopeResolveAsBindingInsideBody(t *testing.T) {
	// A code step inside map[0].item-0.<stepID> references {{ cve }} where
	// the map's as = "cve". The runtime walks ctxPath back, finds map[0].item-0,
	// looks up wf for map[0]'s as-name (= "cve"), matches, returns over[0].
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.cves }}"),
		As:   "cve",
		// Container, Concurrency, Body filled below.
	}
	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Graph: ir.NodeList{mapNode},
	}
	rs := NewRunState("run-x", "digest", nil)
	// Simulate the map executor having recorded the per-item binding.
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: "cve-2026-1234", Status: ""})

	scope := NewScope(rs, wf, "map[0].item-0.triage")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "cve"},
	}})
	if err != nil {
		t.Fatalf("Resolve cve: %v", err)
	}
	if got != "cve-2026-1234" {
		t.Errorf("got %v, want \"cve-2026-1234\"", got)
	}
}

func TestScopeResolveAsBindingIndex(t *testing.T) {
	// {{ <as>.index }} resolves to the N-value REGARDLESS of ItemValue's type.
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.cves }}"),
		As:   "cve",
	}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{mapNode}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 2, ItemValue: "cve-X", Status: ""})

	scope := NewScope(rs, wf, "map[0].item-2.triage")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "cve"}, {Ident: "index"},
	}})
	if err != nil {
		t.Fatalf("Resolve cve.index: %v", err)
	}
	// Numeric: int per the MapItemRecord.N storage type.
	if got != 2 {
		t.Errorf("got %v (%T), want 2 (int)", got, got)
	}
}

func TestScopeResolveAsBindingObjectField(t *testing.T) {
	// {{ <as>.<field> }} when over[N] is a map → descend into the field.
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.cves }}"),
		As:   "cve",
	}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{mapNode}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{
		N:         0,
		ItemValue: map[string]any{"id": "cve-2026-1234", "severity": "high"},
		Status:    "",
	})

	scope := NewScope(rs, wf, "map[0].item-0.triage")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "cve"}, {Ident: "id"},
	}})
	if err != nil {
		t.Fatalf("Resolve cve.id: %v", err)
	}
	if got != "cve-2026-1234" {
		t.Errorf("got %v, want \"cve-2026-1234\"", got)
	}
}

func TestScopeResolveAsBindingOutsideMap(t *testing.T) {
	// Not inside any map ctxPath → unknown ref root error.
	wf := &ir.Workflow{ID: "x", Version: 1}
	rs := NewRunState("run-x", "digest", nil)
	scope := NewScope(rs, wf, "step1")
	_, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "cve"},
	}})
	if err == nil {
		t.Fatal("Resolve cve outside map: err = nil, want non-nil")
	}
	// The existing "unknown ref root" message includes the ident — that's
	// expected; the as-binding resolver returns the same error class when
	// no matching enclosing map is found.
	if !strings.Contains(err.Error(), "unknown ref root") && !strings.Contains(err.Error(), "cve") {
		t.Errorf("err = %v, want mention of \"unknown ref root\" or \"cve\"", err)
	}
}

func TestScopeResolveAsBindingCompositeItemErrors(t *testing.T) {
	// MD-A: bare `{{ <as> }}` on a composite (map/[]any) ItemValue returns
	// an actionable error pointing the author at field/index access. Plain
	// scalars pass through to the caller (template.renderScalar handles them).
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.items }}"),
		As:   "x",
	}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{mapNode}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{
		N:         0,
		ItemValue: map[string]any{"id": "cve-1", "severity": "high"},
		Status:    "",
	})

	scope := NewScope(rs, wf, "map[0].item-0.triage")
	_, err := scope.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "x"}}})
	if err == nil {
		t.Fatal("composite ItemValue bare ref: err = nil, want non-nil")
	}
	msg := err.Error()
	for _, want := range []string{"composite", "x.<field>", "x.index"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q, missing actionable hint %q", msg, want)
		}
	}
	// Available-fields hint includes sorted keys.
	if !strings.Contains(msg, "id") || !strings.Contains(msg, "severity") {
		t.Errorf("err = %q, want listing of fields [id severity]", msg)
	}

	// Array ItemValue case.
	rs.RecordMapItem("map[0]", MapItemRecord{
		N: 1, ItemValue: []any{"a", "b", "c"}, Status: "",
	})
	scope2 := NewScope(rs, wf, "map[0].item-1.triage")
	_, err = scope2.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "x"}}})
	if err == nil {
		t.Fatal("array ItemValue bare ref: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "array") || !strings.Contains(err.Error(), "x.0") {
		t.Errorf("err = %q, want mention of \"array\" and indexing hint", err)
	}

	// Scalar ItemValue still passes through.
	rs.RecordMapItem("map[0]", MapItemRecord{
		N: 2, ItemValue: "scalar-value", Status: "",
	})
	scope3 := NewScope(rs, wf, "map[0].item-2.triage")
	got, err := scope3.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "x"}}})
	if err != nil {
		t.Fatalf("scalar ItemValue bare ref: err = %v, want nil", err)
	}
	if got != "scalar-value" {
		t.Errorf("scalar ItemValue: got %v, want \"scalar-value\"", got)
	}
}

func TestScopeResolveAsBindingItemValueNotBoundErrors(t *testing.T) {
	// Defense-in-depth (Design Q3): if MapItemRecord exists for N=0 but
	// ItemValue is nil (Fold-rebuilt without runtime re-eval), the
	// resolver MUST error rather than silently return nil.
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.cves }}"),
		As:   "cve",
	}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{mapNode}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: nil, Status: ItemPassed})

	scope := NewScope(rs, wf, "map[0].item-0.triage")
	_, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "cve"},
	}})
	if err == nil {
		t.Fatal("Resolve cve with nil ItemValue: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "value not bound") {
		t.Errorf("err = %v, want mention of \"value not bound\" (runtime invariant violation)", err)
	}
}

func TestScopeResolveAsBindingShadowsReservedRoots(t *testing.T) {
	// Design Q4: reserved root cases (run/input/step/evaluate) win over
	// <as> bindings. Author who writes `as: input` is silently shadowed.
	// This test PINS the behavior so a refactor that swaps order is caught.
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.foo }}"),
		As:   "input", // ← shadows reserved root; runtime ignores the as binding
	}
	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Input: &ir.JSONSchema{
			"type": "object",
			"properties": map[string]any{
				"foo": map[string]any{"type": "string"},
			},
		},
		Graph: ir.NodeList{mapNode},
	}
	rs := NewRunState("run-x", "digest", map[string]any{"foo": "from-workflow-input"})
	rs.RecordMapItem("map[0]", MapItemRecord{
		N: 0, ItemValue: map[string]any{"foo": "from-map-binding"}, Status: "",
	})

	scope := NewScope(rs, wf, "map[0].item-0.triage")
	got, err := scope.Resolve(&template.Ref{Segments: []template.Segment{
		{Ident: "input"}, {Ident: "foo"},
	}})
	if err != nil {
		t.Fatalf("Resolve input.foo: %v", err)
	}
	// Reserved root wins: got the workflow input, NOT the map binding.
	if got != "from-workflow-input" {
		t.Errorf("got %v, want \"from-workflow-input\" (reserved root wins; Design Q4)", got)
	}
}

func TestScopeResolveAsBindingNestedMaps(t *testing.T) {
	// Inner map inside outer map's body. Inner as = "host", outer as = "cve".
	// ctxPath: "map[0].item-0.map[1].item-2.scan_step"
	// {{ host }} → innermost map's binding.
	// {{ cve }} → outer map's binding (walk continues past inner).
	outerMap := &ir.Map{
		As:   "cve",
		Over: ir.Expr("{{ input.cves }}"),
		Body: ir.NodeList{
			&ir.Map{
				As:   "host",
				Over: ir.Expr("{{ input.hosts }}"),
			},
		},
	}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{outerMap}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: "cve-A", Status: ""})
	// The inner map's path under outer's item 0 is "map[0].item-0.map[0]"
	// because ir.PathFor uses positional indices within the parent body.
	// (See engine/path_test.go for the canonical examples.)
	rs.RecordMapItem("map[0].item-0.map[0]", MapItemRecord{N: 2, ItemValue: "host-X", Status: ""})

	scope := NewScope(rs, wf, "map[0].item-0.map[0].item-2.scan_step")
	gotHost, err := scope.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "host"}}})
	if err != nil {
		t.Fatalf("Resolve host: %v", err)
	}
	if gotHost != "host-X" {
		t.Errorf("inner: got %v, want \"host-X\"", gotHost)
	}
	gotCve, err := scope.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "cve"}}})
	if err != nil {
		t.Fatalf("Resolve cve: %v", err)
	}
	if gotCve != "cve-A" {
		t.Errorf("outer-from-inner-ctx: got %v, want \"cve-A\"", gotCve)
	}
}

func TestRuntimeMapPathToStatic(t *testing.T) {
	// CR-A fix: pins the conversion rule (.item-K preceded by map[X] →
	// .body) for the walker's static-idx lookup. Covers nested maps, leaves
	// loop iter-K untouched, and doesn't false-positive on step IDs that
	// happen to match "item-\d+".
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"top-level map (identity — no item-K to convert)",
			"map[0]",
			"map[0]",
		},
		{
			"map at item-K boundary: item-K is converted to body",
			"map[0].item-0",
			"map[0].body",
		},
		{
			"nested map: inner's runtime mapPath converts cleanly",
			"map[0].item-0.map[0]",
			"map[0].body.map[0]",
		},
		{
			"two nested item-K segments both convert",
			"map[0].item-0.map[0].item-2",
			"map[0].body.map[0].body",
		},
		{
			"loop iter inside map.body: iter-K untouched, item-K converts",
			"map[0].item-0.loop[0].body.iter-3",
			"map[0].body.loop[0].body.iter-3",
		},
		{
			"step id literally 'item-3' inside map.body NOT preceded by map[X]",
			"map[0].item-0.process.item-3",
			// segs: [map[0], item-0, process, item-3]
			//   i=1: item-0 after map[0] → body
			//   i=2: process — not item-, skip
			//   i=3: item-3 after "process" (not "map["), skip
			"map[0].body.process.item-3",
		},
		{
			"map nested inside loop body: no item-K-after-map[X] pattern",
			"loop[0].body.iter-1.map[0]",
			"loop[0].body.iter-1.map[0]",
		},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runtimeMapPathToStatic(c.in)
			if got != c.want {
				t.Errorf("runtimeMapPathToStatic(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEnclosingMapForBindingTable(t *testing.T) {
	// Walker table — confirms the segment-pair match logic.
	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Graph: ir.NodeList{
			&ir.Map{As: "cve"},
			// Other nodes at top level don't affect map[0]'s lookup.
		},
	}
	idx := mapPathIndex(wf)
	cases := []struct {
		name     string
		ctxPath  string
		asName   string
		wantPath string
		wantN    int
		wantOk   bool
	}{
		{"empty ctxPath", "", "cve", "", 0, false},
		{"plain step", "step1", "cve", "", 0, false},
		{"map ident alone", "map[0]", "cve", "", 0, false},
		{"map + item", "map[0].item-0", "cve", "map[0]", 0, true},
		{"map + item + step", "map[0].item-0.triage", "cve", "map[0]", 0, true},
		{"map + item + deep", "map[0].item-3.try[0].do.echo", "cve", "map[0]", 3, true},
		{"wrong asName", "map[0].item-0.triage", "host", "", 0, false}, // map[0]'s as is "cve", not "host"
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPath, gotN, ok := enclosingMapForBinding(c.ctxPath, idx, c.asName)
			if gotPath != c.wantPath || gotN != c.wantN || ok != c.wantOk {
				t.Errorf("enclosingMapForBinding(%q, idx, %q) = (%q, %d, %v); want (%q, %d, %v)",
					c.ctxPath, c.asName, gotPath, gotN, ok, c.wantPath, c.wantN, c.wantOk)
			}
		})
	}
}

func TestResolveAggregateMapOutputs(t *testing.T) {
	rs := NewRunState("run-x", "digest-x", nil)
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{
		&ir.Map{Over: "{{ step.seed.items }}", As: "u", Container: "c", Concurrency: 1, Body: ir.NodeList{
			&ir.AgentStep{ID: "scan", Container: "c",
				OutputSchema: &ir.JSONSchema{"type": "object", "required": []any{"finding"}, "properties": map[string]any{"finding": map[string]any{"type": "string"}}, "additionalProperties": false}}}}}}
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: "a", Status: ItemPassed})
	rs.RecordMapItem("map[0]", MapItemRecord{N: 2, ItemValue: "c", Status: ItemPassed}) // N=1 absent → compaction
	rs.RecordCompleted("map[0].item-0.scan", NodeResult{Outputs: map[string]any{"finding": "A"}})
	rs.RecordCompleted("map[0].item-2.scan", NodeResult{Outputs: map[string]any{"finding": "C"}})
	s := NewScope(rs, wf, "map[1].over") // ref site OUTSIDE map[0]
	whole, err := s.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "scan"}}})
	if err != nil || len(whole.([]any)) != 2 {
		t.Fatalf("whole aggregate: %v / %#v", err, whole)
	}
	proj, err := s.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "scan"}, {Ident: "finding"}}})
	parr, _ := proj.([]any)
	if err != nil || len(parr) != 2 || parr[0] != "A" || parr[1] != "C" {
		t.Fatalf("field aggregate: %v / %#v", err, proj)
	}
}
