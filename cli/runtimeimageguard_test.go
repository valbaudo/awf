package cli

import (
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

func runtimeImageWF() *ir.Workflow {
	return &ir.Workflow{
		ID: "p6a", Version: 1,
		Containers: map[string]ir.Container{"vl": {Resources: &ir.Resources{CPU: "1"}}},
		Graph: ir.NodeList{
			&ir.Map{Over: "{{ input.items }}", As: "v", Container: "vl", Image: "{{ v.image }}", Concurrency: intPtr(1),
				Body: ir.NodeList{&ir.CodeStep{ID: "probe", Container: "vl", Run: "true"}}},
		},
	}
}

func TestRuntimeImageGuardRejectsImageIgnoringBackend(t *testing.T) {
	nat, err := native.New(t.TempDir()) // native advertises RuntimeImage:false
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	if err := checkRuntimeImageCapability(runtimeImageWF(), nat); err == nil {
		t.Error("guard accepted a map.image workflow on native; want rejection")
	}
}

func TestRuntimeImageGuardAcceptsCapableBackend(t *testing.T) {
	if err := checkRuntimeImageCapability(runtimeImageWF(), container.NewFake()); err != nil {
		t.Errorf("guard rejected a map.image workflow on a capable backend: %v", err)
	}
}

func TestRuntimeImageGuardIgnoresStaticWorkflow(t *testing.T) {
	nat, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	wf := &ir.Workflow{ID: "static", Version: 1,
		Containers: map[string]ir.Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      ir.NodeList{&ir.CodeStep{ID: "a", Container: "c", Run: "true"}}}
	if err := checkRuntimeImageCapability(wf, nat); err != nil {
		t.Errorf("guard rejected a static workflow on native: %v", err)
	}
}
