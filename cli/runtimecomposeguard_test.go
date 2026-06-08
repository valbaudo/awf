package cli

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

func runtimeComposeGuardWF() *ir.Workflow {
	return &ir.Workflow{
		ID: "runtime-compose", Version: 1,
		Containers: map[string]ir.Container{
			"runner": {Image: "oci://example.com/runner@sha256:" + strings.Repeat("0", 64)},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:          "lab_gen",
				Container:   "runner",
				Run:         "true",
				OutputFiles: ir.OutputFiles{{Name: "compose", Path: "/work/compose.yml"}},
			},
			&ir.Compose{
				As: "lab", From: "step.lab_gen.files.compose", Service: "web",
				Body: ir.NodeList{&ir.CodeStep{ID: "smoke", Container: "lab", Run: "true"}},
			},
		},
	}
}

func TestRuntimeComposeGuardRejectsNativeWithPath(t *testing.T) {
	nat, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	err = checkRuntimeComposeCapability(runtimeComposeGuardWF(), "native", nat)
	if err == nil {
		t.Fatal("guard accepted a runtime compose workflow on native; want rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "native") || !strings.Contains(msg, "compose[1]") {
		t.Fatalf("guard message = %q, want native backend and first compose path", msg)
	}
}

func TestRuntimeComposeGuardAcceptsCapableBackend(t *testing.T) {
	if err := checkRuntimeComposeCapability(runtimeComposeGuardWF(), "fake", container.NewFake()); err != nil {
		t.Errorf("guard rejected runtime compose on a capable backend: %v", err)
	}
}

func TestRuntimeComposeGuardIgnoresStaticWorkflow(t *testing.T) {
	nat, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	wf := &ir.Workflow{ID: "static", Version: 1,
		Containers: map[string]ir.Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      ir.NodeList{&ir.CodeStep{ID: "a", Container: "c", Run: "true"}}}
	if err := checkRuntimeComposeCapability(wf, "native", nat); err != nil {
		t.Errorf("guard rejected a static workflow on native: %v", err)
	}
}
