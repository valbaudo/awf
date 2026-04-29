package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// stubAdapter is a minimal Adapter for Registry tests — defined here so the
// Registry test doesn't depend on agent/fake (which is tested by its own
// fake_test.go).
type stubAdapter struct{ ref string }

func (s *stubAdapter) Ref() string                                                { return s.ref }
func (s *stubAdapter) Capabilities() agent.Caps                                   { return agent.Caps{} }
func (s *stubAdapter) Version(context.Context, container.Handle) (string, error) { return "v0", nil }
func (s *stubAdapter) ValidateConfig(ir.RawConfig) error                          { return nil }
func (s *stubAdapter) Launch(context.Context, container.Handle, agent.AgentInvocation) (agent.AgentResult, <-chan agent.AgentEvent, error) {
	return agent.AgentResult{}, nil, nil
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	var r agent.Registry
	a := &stubAdapter{ref: "anthropic/claude-code"}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("anthropic/claude-code")
	if !ok {
		t.Fatalf("Lookup: not found")
	}
	if got != a {
		t.Errorf("Lookup returned different adapter: got %v, want %v", got, a)
	}
}

func TestRegistry_LookupMissing(t *testing.T) {
	var r agent.Registry
	if got, ok := r.Lookup("nonexistent"); ok || got != nil {
		t.Errorf("Lookup(\"nonexistent\") = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	var r agent.Registry
	a1 := &stubAdapter{ref: "x"}
	a2 := &stubAdapter{ref: "x"}
	if err := r.Register(a1); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(a2)
	var dup *agent.ErrAdapterAlreadyRegistered
	if !errors.As(err, &dup) {
		t.Fatalf("second Register returned %v; want *ErrAdapterAlreadyRegistered", err)
	}
	if dup.Ref != "x" {
		t.Errorf("Ref = %q, want %q", dup.Ref, "x")
	}
}

func TestRegistry_RegisterRejectsEmptyRef(t *testing.T) {
	var r agent.Registry
	a := &stubAdapter{ref: ""}
	err := r.Register(a)
	if err == nil {
		t.Fatalf("Register with empty Ref: err = nil, want non-nil")
	}
}

func TestRegistry_Refs(t *testing.T) {
	var r agent.Registry
	if err := r.Register(&stubAdapter{ref: "b"}); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	if err := r.Register(&stubAdapter{ref: "a"}); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	refs := r.Refs()
	if len(refs) != 2 {
		t.Fatalf("len(Refs) = %d, want 2", len(refs))
	}
	// Refs MUST be sorted for deterministic output (used by Phase 6 obs).
	if refs[0] != "a" || refs[1] != "b" {
		t.Errorf("Refs = %v, want [a b] (sorted)", refs)
	}
}

func TestRegistry_ZeroValueIsUsable(t *testing.T) {
	// var r agent.Registry — no New() needed; zero value is usable.
	var r agent.Registry
	if err := r.Register(&stubAdapter{ref: "x"}); err != nil {
		t.Fatalf("zero-value Register: %v", err)
	}
}

// Verify Registry implements Resolver (compile-time check).
var _ agent.Resolver = (*agent.Registry)(nil)
