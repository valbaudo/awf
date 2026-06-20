package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/ir"
)

// credFake wraps a *fake.Fake and additionally implements agent.CredentialNamer.
type credFake struct {
	*fake.Fake
	requiredEnv []string
}

func (c *credFake) RequiredEnv() []string { return c.requiredEnv }

func newCredReg(t *testing.T, ref string, envs []string) *agent.Registry {
	t.Helper()
	fk := &credFake{
		Fake:        fake.New(ref),
		requiredEnv: envs,
	}
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// oneStepWF returns a minimal workflow with one agent step using the given ref.
func oneStepWF(ref string) *ir.Workflow {
	return &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "step1", Uses: ref, Container: "lab"},
	}}
}

// TestCredentialGuard_NoneSet_Warns: when NONE of the adapter's RequiredEnv
// vars is set in the host env, checkCredentialPresence must print a warning
// naming the adapter and return nil (advisory, non-fatal).
func TestCredentialGuard_NoneSet_Warns(t *testing.T) {
	const ref = "test/agent"
	// Use env vars that are virtually guaranteed to not be set in test env.
	envs := []string{"__AWF_TEST_CRED_A__", "__AWF_TEST_CRED_B__"}
	reg := newCredReg(t, ref, envs)
	wf := oneStepWF(ref)
	ld := &ir.LoadedDefinition{Workflow: wf}

	var stderr bytes.Buffer
	err := checkCredentialPresence(ld, reg, &stderr)
	if err != nil {
		t.Fatalf("checkCredentialPresence returned non-nil error %v, want nil (advisory)", err)
	}
	out := stderr.String()
	if !strings.Contains(out, ref) {
		t.Errorf("warning missing adapter ref %q; got: %q", ref, out)
	}
	if !strings.Contains(out, "__AWF_TEST_CRED_A__") {
		t.Errorf("warning missing env var name; got: %q", out)
	}
}

// TestCredentialGuard_OneSet_NoWarn: when at least one of the adapter's
// RequiredEnv vars IS set, no warning is emitted (provider-alternative OK).
func TestCredentialGuard_OneSet_NoWarn(t *testing.T) {
	const ref = "test/agent"
	envs := []string{"__AWF_TEST_CRED_A__", "__AWF_TEST_CRED_B__"}
	reg := newCredReg(t, ref, envs)
	wf := oneStepWF(ref)
	ld := &ir.LoadedDefinition{Workflow: wf}

	// Set one of the two — should suppress the warning.
	t.Setenv("__AWF_TEST_CRED_A__", "sk-testvalue")

	var stderr bytes.Buffer
	err := checkCredentialPresence(ld, reg, &stderr)
	if err != nil {
		t.Fatalf("checkCredentialPresence returned non-nil error %v, want nil", err)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning when one env is set; got: %q", out)
	}
}

// TestCredentialGuard_NoCredentialNamer_NoWarn: an adapter that does NOT
// implement CredentialNamer is silently skipped (no warning, no error).
func TestCredentialGuard_NoCredentialNamer_NoWarn(t *testing.T) {
	const ref = "test/plain-agent"
	// plain fake.Fake does not implement CredentialNamer.
	fk := fake.New(ref)
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	wf := oneStepWF(ref)
	ld := &ir.LoadedDefinition{Workflow: wf}

	var stderr bytes.Buffer
	err := checkCredentialPresence(ld, &reg, &stderr)
	if err != nil {
		t.Fatalf("checkCredentialPresence returned non-nil error %v, want nil", err)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning for non-CredentialNamer adapter; got: %q", out)
	}
}

// TestCredentialGuard_SameAdapterMultipleSteps_DedupsWarning: the same adapter
// used in two steps produces exactly one warning (dedup per adapter ref).
func TestCredentialGuard_SameAdapterMultipleSteps_DedupsWarning(t *testing.T) {
	const ref = "test/agent"
	envs := []string{"__AWF_TEST_CRED_A__"}
	reg := newCredReg(t, ref, envs)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "s1", Uses: ref, Container: "lab"},
		&ir.AgentStep{ID: "s2", Uses: ref, Container: "lab"},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}

	var stderr bytes.Buffer
	err := checkCredentialPresence(ld, reg, &stderr)
	if err != nil {
		t.Fatalf("checkCredentialPresence returned non-nil error %v, want nil", err)
	}
	out := stderr.String()
	// Count occurrences of the ref in the warning output — should be exactly 1.
	if count := strings.Count(out, ref); count != 1 {
		t.Errorf("expected exactly 1 warning for deduped adapter %q; got %d occurrences in %q", ref, count, out)
	}
}
