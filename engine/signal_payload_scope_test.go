package engine

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/template"
)

func TestPayloadScopeResolvesBareIdent(t *testing.T) {
	s := newPayloadScope(map[string]any{"candidate_id": "h-7", "score": float64(3)})
	ref, err := template.ParseRef("candidate_id")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "h-7" {
		t.Errorf("got %v, want h-7", v)
	}
}

func TestPayloadScopeResolvesNestedField(t *testing.T) {
	s := newPayloadScope(map[string]any{"meta": map[string]any{"id": "x"}})
	ref, _ := template.ParseRef("meta.id")
	v, err := s.Resolve(ref)
	if err != nil || v != "x" {
		t.Errorf("got (%v, %v), want (x, nil)", v, err)
	}
}

func TestPayloadScopeUnknownFieldUnresolved(t *testing.T) {
	s := newPayloadScope(map[string]any{"a": 1})
	ref, _ := template.ParseRef("missing")
	_, err := s.Resolve(ref)
	var ee *template.EvalError
	if err == nil || !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Errorf("err = %v, want AWF4002 unresolved", err)
	}
}
