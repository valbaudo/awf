package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/ir"
)

// newLivenessReg registers a plain fake adapter under ref with the given
// measured SurfacesLiveness tier.
func newLivenessReg(t *testing.T, ref string, tier agent.Liveness) *agent.Registry {
	t.Helper()
	fk := fake.New(ref).WithCaps(agent.Caps{SurfacesLiveness: tier})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// idleDurPtr builds a *ir.Duration for a step's timeout.idle.
func idleDurPtr(d time.Duration) *ir.Duration {
	v := ir.Duration(d)
	return &v
}

// oneStepIdleWF returns a one-agent-step workflow using ref; idle sets the
// step's timeout.idle when non-zero (a nil timeout otherwise).
func oneStepIdleWF(ref string, idle time.Duration) *ir.Workflow {
	step := &ir.AgentStep{ID: "gen", Uses: ref, Container: "lab"}
	if idle != 0 {
		step.Timeout = &ir.Timeout{Idle: idleDurPtr(idle)}
	}
	return &ir.Workflow{Graph: ir.NodeList{step}}
}

// TestIdleLivenessGuard_NoneWithIdle_Warns: a blind (SurfacesLiveness=None)
// adapter with an author-set timeout.idle must warn — the adapter surfaces no
// liveness, so idle degenerates to a wall-clock deadline. Non-fatal (returns nil).
func TestIdleLivenessGuard_NoneWithIdle_Warns(t *testing.T) {
	const ref = "test/blind"
	reg := newLivenessReg(t, ref, agent.LivenessNone)
	ld := &ir.LoadedDefinition{Workflow: oneStepIdleWF(ref, 20*time.Second)}

	var stderr bytes.Buffer
	if err := checkIdleLiveness(ld, reg, &stderr); err != nil {
		t.Fatalf("checkIdleLiveness returned non-nil error %v, want nil (advisory)", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "AWF3016") {
		t.Errorf("warning missing code AWF3016; got: %q", out)
	}
	if !strings.Contains(out, ref) {
		t.Errorf("warning missing adapter ref %q; got: %q", ref, out)
	}
	if !strings.Contains(out, "gen") {
		t.Errorf("warning missing step id %q; got: %q", "gen", out)
	}
}

// TestIdleLivenessGuard_CoarseWithIdle_NoWarn: a Coarse-tier adapter surfaces
// enough liveness for idle to mean something — no warning even with idle set.
func TestIdleLivenessGuard_CoarseWithIdle_NoWarn(t *testing.T) {
	const ref = "test/coarse"
	reg := newLivenessReg(t, ref, agent.LivenessCoarse)
	ld := &ir.LoadedDefinition{Workflow: oneStepIdleWF(ref, 20*time.Second)}

	var stderr bytes.Buffer
	if err := checkIdleLiveness(ld, reg, &stderr); err != nil {
		t.Fatalf("checkIdleLiveness returned non-nil error %v, want nil", err)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning for Coarse tier with idle; got: %q", out)
	}
}

// TestIdleLivenessGuard_FineWithIdle_NoWarn: a Fine-tier adapter must not warn.
func TestIdleLivenessGuard_FineWithIdle_NoWarn(t *testing.T) {
	const ref = "test/fine"
	reg := newLivenessReg(t, ref, agent.LivenessFine)
	ld := &ir.LoadedDefinition{Workflow: oneStepIdleWF(ref, 20*time.Second)}

	var stderr bytes.Buffer
	if err := checkIdleLiveness(ld, reg, &stderr); err != nil {
		t.Fatalf("checkIdleLiveness returned non-nil error %v, want nil", err)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning for Fine tier with idle; got: %q", out)
	}
}

// TestIdleLivenessGuard_NoneNoIdle_NoWarn: a blind adapter WITHOUT an author
// idle: must not warn — the author never opted into a tight idle, so there is
// nothing to advise.
func TestIdleLivenessGuard_NoneNoIdle_NoWarn(t *testing.T) {
	const ref = "test/blind"
	reg := newLivenessReg(t, ref, agent.LivenessNone)
	ld := &ir.LoadedDefinition{Workflow: oneStepIdleWF(ref, 0)}

	var stderr bytes.Buffer
	if err := checkIdleLiveness(ld, reg, &stderr); err != nil {
		t.Fatalf("checkIdleLiveness returned non-nil error %v, want nil", err)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning for None tier without idle; got: %q", out)
	}
}
