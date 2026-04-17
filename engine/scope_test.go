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
