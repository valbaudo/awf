package engine

import (
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

func TestToolImplScopeResolvesArgs(t *testing.T) {
	rs := NewRunState("r", "d", map[string]any{"q": "hi"})
	wf := &ir.Workflow{ID: "wf", Version: 1}
	s := newToolImplScope(rs, wf, "react[0].round-1.tool-0", "/awf/args/r1-t0.json",
		map[string]any{"x": 7, "obj": map[string]any{}})

	v, err := s.Resolve(mustParseRef(t, "args_file"))
	if err != nil || v != "/awf/args/r1-t0.json" {
		t.Fatalf("args_file = %v, %v", v, err)
	}
	v, err = s.Resolve(mustParseRef(t, "args.x"))
	if err != nil || v != 7 {
		t.Fatalf("args.x = %v, %v", v, err)
	}
	// non-scalar arg is absent (best-effort): args.obj → unresolved
	if _, err := s.Resolve(mustParseRef(t, "args.obj")); err == nil {
		t.Fatal("args.obj (non-scalar) should be unresolved")
	}
	// a scalar arg that simply isn't present is unresolved too.
	if _, err := s.Resolve(mustParseRef(t, "args.missing")); err == nil {
		t.Fatal("args.missing should be unresolved")
	}
	// input.* delegates to base
	v, err = s.Resolve(mustParseRef(t, "input.q"))
	if err != nil || v != "hi" {
		t.Fatalf("input.q via base = %v, %v", v, err)
	}
	// an unknown root still errors via the base scope.
	if _, err := s.Resolve(mustParseRef(t, "nope.field")); err == nil {
		t.Fatal("unknown root should error via base")
	}
}

func TestToolImplScopeArgsFloatScalar(t *testing.T) {
	// JSON numbers arrive as float64 after json.Unmarshal — the common case for
	// {{ args.<field> }} bound from raw model-emitted arguments.
	rs := NewRunState("r", "d", nil)
	wf := &ir.Workflow{ID: "wf", Version: 1}
	s := newToolImplScope(rs, wf, "react[0].round-1.tool-0", "/x.json",
		map[string]any{"n": float64(42), "name": "abc", "flag": true})
	for _, tc := range []struct {
		ref  string
		want any
	}{
		{"args.n", float64(42)},
		{"args.name", "abc"},
		{"args.flag", true},
	} {
		got, err := s.Resolve(mustParseRef(t, tc.ref))
		if err != nil || got != tc.want {
			t.Fatalf("%s = %v, %v; want %v", tc.ref, got, err, tc.want)
		}
	}
}

var _ template.Scope = (*toolImplScope)(nil)
