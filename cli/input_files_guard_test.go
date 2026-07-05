package cli

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/ir"
)

// newInputFilesReg registers a fake adapter under ref with Containerless
// always true (the guard only fires on containerless steps) and the given
// InlineInputFiles bit.
func newInputFilesReg(t *testing.T, ref string, inline bool) *agent.Registry {
	t.Helper()
	fk := fake.New(ref).WithCaps(agent.Caps{Containerless: true, InlineInputFiles: inline})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// TestInputFilesGuard_ContainerlessNonInlineRejected: a containerless agent
// step (no container:) declaring input_files against an adapter that does
// NOT inline them (codexlive-like) must be rejected — otherwise the files
// are silently dropped at Launch.
func TestInputFilesGuard_ContainerlessNonInlineRejected(t *testing.T) {
	const ref = "test/codexlive-like"
	reg := newInputFilesReg(t, ref, false)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "step1", Uses: ref, InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}

	err := checkInputFilesForLoadedDefinition(ld, reg, nil)
	if err == nil {
		t.Fatal("checkInputFilesForLoadedDefinition returned nil, want an error for containerless step with input_files against a non-inline adapter")
	}
}

// TestInputFilesGuard_ContainerlessInlineAllowed: same shape, but the
// resolved adapter DOES inline input_files (awf/llm-like) — no error.
func TestInputFilesGuard_ContainerlessInlineAllowed(t *testing.T) {
	const ref = "test/awfllm-like"
	reg := newInputFilesReg(t, ref, true)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "step1", Uses: ref, InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}

	if err := checkInputFilesForLoadedDefinition(ld, reg, nil); err != nil {
		t.Fatalf("checkInputFilesForLoadedDefinition = %v, want nil (adapter inlines input_files)", err)
	}
}

// TestInputFilesGuard_ContainerBackedAllowed: a step WITH container: and
// input_files is fine against ANY adapter (including a non-inline one) —
// container staging handles delivery, not the containerless inline path.
func TestInputFilesGuard_ContainerBackedAllowed(t *testing.T) {
	const ref = "test/codexlive-like"
	reg := newInputFilesReg(t, ref, false)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "step1", Uses: ref, Container: "lab", InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}

	if err := checkInputFilesForLoadedDefinition(ld, reg, nil); err != nil {
		t.Fatalf("checkInputFilesForLoadedDefinition = %v, want nil (container-backed staging handles input_files)", err)
	}
}
