package cli

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/ir"
)

// withGuardFake wraps *fake.Fake (permissive by default) and overrides
// ValidateConfig to exercise the two ErrInvalidConfig shapes the run-start
// with:-config guard must distinguish:
//
//   - an unknown key "promt" (a typo of "prompt") → KeyUnknown: true. A key
//     name can never be fixed by templating, so the guard always surfaces it.
//   - key "effort" set to anything other than "low"/"medium"/"high" →
//     KeyUnknown: false, a value-shape error. Pre-substitution this rejects
//     BOTH a literal bad value ("bogus") and a not-yet-substituted template
//     ("{{x}}") identically — it's the GUARD's suppressTemplatedValueErr,
//     not this stub, that must tell them apart.
type withGuardFake struct {
	*fake.Fake
}

func (f *withGuardFake) ValidateConfig(with ir.RawConfig) error {
	if _, ok := with["promt"]; ok {
		return &agent.ErrInvalidConfig{Ref: f.Ref(), Key: "promt", Reason: "unknown key", KeyUnknown: true}
	}
	if v, ok := with["effort"]; ok && v != "low" && v != "medium" && v != "high" {
		return &agent.ErrInvalidConfig{Ref: f.Ref(), Key: "effort", Reason: "must be low, medium, or high", KeyUnknown: false}
	}
	return nil
}

func newWithGuardReg(t *testing.T, ref string) *agent.Registry {
	t.Helper()
	var reg agent.Registry
	if err := reg.Register(&withGuardFake{Fake: fake.New(ref)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// withGuardWF returns a minimal loaded definition with one agent step using
// ref and the given with: config.
func withGuardWF(ref string, with ir.RawConfig) *ir.LoadedDefinition {
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "step1", Uses: ref, Container: "lab", With: with},
	}}
	return &ir.LoadedDefinition{Workflow: wf}
}

// TestWithGuard_UnknownKeyRejected: a typo'd/unknown with: key (promt) is
// always rejected, even with no templating involved.
func TestWithGuard_UnknownKeyRejected(t *testing.T) {
	const ref = "test/agent"
	reg := newWithGuardReg(t, ref)
	ld := withGuardWF(ref, ir.RawConfig{"promt": "do the thing"})

	err := checkWithConfigForLoadedDefinition(ld, reg, nil)
	if err == nil {
		t.Fatal("checkWithConfigForLoadedDefinition returned nil, want an error for unknown key \"promt\"")
	}
}

// TestWithGuard_TemplatedValueSuppressed: a value-shape rejection on a key
// whose value is a not-yet-substituted template ("{{x}}") is suppressed —
// the guard cannot validate a template's eventual value at run start.
func TestWithGuard_TemplatedValueSuppressed(t *testing.T) {
	const ref = "test/agent"
	reg := newWithGuardReg(t, ref)
	ld := withGuardWF(ref, ir.RawConfig{"effort": "{{x}}"})

	err := checkWithConfigForLoadedDefinition(ld, reg, nil)
	if err != nil {
		t.Fatalf("checkWithConfigForLoadedDefinition = %v, want nil (templated value must be suppressed)", err)
	}
}

// TestWithGuard_LiteralBadValueRejected: the identical value-shape rejection
// on a LITERAL (non-templated) bad value is NOT suppressed.
func TestWithGuard_LiteralBadValueRejected(t *testing.T) {
	const ref = "test/agent"
	reg := newWithGuardReg(t, ref)
	ld := withGuardWF(ref, ir.RawConfig{"effort": "bogus"})

	err := checkWithConfigForLoadedDefinition(ld, reg, nil)
	if err == nil {
		t.Fatal("checkWithConfigForLoadedDefinition returned nil, want an error for literal bad value \"bogus\"")
	}
}

// TestWithGuard_UnknownKeyRejectedEvenWhenTemplatedValue: KeyUnknown wins —
// an unknown key is rejected even when ITS value happens to be templated
// (a key name typo is never fixed by template substitution).
func TestWithGuard_UnknownKeyRejectedEvenWhenTemplatedValue(t *testing.T) {
	const ref = "test/agent"
	reg := newWithGuardReg(t, ref)
	ld := withGuardWF(ref, ir.RawConfig{"promt": "{{x}}"})

	err := checkWithConfigForLoadedDefinition(ld, reg, nil)
	if err == nil {
		t.Fatal("checkWithConfigForLoadedDefinition returned nil, want an error (KeyUnknown must win over template-suppression)")
	}
}
