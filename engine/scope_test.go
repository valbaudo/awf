package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// Compile-time check: engine.Scope satisfies template.Scope.
var _ template.Scope = (*Scope)(nil)

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
			"lab": {Image: "oci://example@sha256:" + strRepeat("a", 64)},
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

func strRepeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
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
		RunID: "run-123",
		Input: map[string]any{"cve_id": "CVE-2024-9999", "flag": true},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")

	ref := mustParseRef(t, "run.id")
	v, err := sc.Resolve(ref)
	if err != nil || v != "run-123" {
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
	exit := 0
	rs := &RunState{
		RunID:     "run-X",
		LoopIters: map[string]int{"loop[1]": 2},
		Completed: map[string]NodeResult{
			"loop[1].body.iter-1.echo": {Outcome: OutcomeOK, ExitCode: &exit, Outputs: map[string]any{"n": 1.0}},
			"loop[1].body.iter-2.echo": {Outcome: OutcomeOK, ExitCode: &exit, Outputs: map[string]any{"n": 2.0}},
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
	exit := 0
	rs := &RunState{
		RunID:     "run-X",
		LoopIters: map[string]int{"loop[1]": 2}, // 2 iters completed, iter-3 is running
		Completed: map[string]NodeResult{
			"loop[1].body.iter-2.echo": {Outcome: OutcomeOK, ExitCode: &exit, Outputs: map[string]any{"n": 2.0}},
			"loop[1].body.iter-3.echo": {Outcome: OutcomeOK, ExitCode: &exit, Outputs: map[string]any{"n": 3.0}}, // committed earlier in iter-3
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
	exit := 0
	rs := &RunState{
		RunID: "run-X",
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: &exit, Outputs: map[string]any{"web_exploitable": true}},
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
	rs := &RunState{RunID: "run-X"}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "triage")
	ref := mustParseRef(t, "step.ghost.field")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002", err)
	}
}

func TestScopeResolveStepInLoopWithNoIterIsAWF4002(t *testing.T) {
	// A ref to a step inside a loop, where the loop has zero completed iters
	// AND ctxPath is outside the loop: there is no value to return.
	rs := &RunState{RunID: "run-X", LoopIters: map[string]int{}}
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
	rs := &RunState{RunID: "run-X"}
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

func TestScopeResolveStepStdoutIsDeferredToSlice24(t *testing.T) {
	// step.<id>.stdout resolution is deferred to slice 2.4 (Design question 2
	// in the plan). The runtime returns AWF4099 (deferred-to-later-slice) so
	// authors get a clear "not yet" message rather than a silent empty value.
	exit := 0
	rs := &RunState{
		RunID: "run-X",
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, ExitCode: &exit},
		},
	}
	wf := minimalWorkflow()
	sc := NewScope(rs, wf, "loop[1]")
	ref := mustParseRef(t, "step.triage.stdout")
	_, err := sc.Resolve(ref)
	var ee *template.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *template.EvalError: %v", err, err)
	}
	if ee.Code != template.EvalCodeDeferred {
		t.Errorf("err.Code = %q, want %q (AWF4099)", ee.Code, template.EvalCodeDeferred)
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
		Containers: map[string]ir.Container{"lab": {Image: "oci://example@sha256:" + strRepeat("a", 64)}},
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
	rs := &RunState{RunID: "run-X"}
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
		RunID: "run-X",
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

func mustParseRef(t *testing.T, src string) *template.Ref {
	t.Helper()
	r, err := template.ParseRef(src)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", src, err)
	}
	return r
}
