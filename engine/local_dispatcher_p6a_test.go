package engine

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestContainerSpecForResourcesOnly(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"version_lab": {Resources: &ir.Resources{CPU: "2", Mem: "4Gi"}},
		},
	}
	spec := ContainerSpecFor(wf, nil, "version_lab")
	if spec.Image != "" {
		t.Errorf("spec.Image = %q, want empty", spec.Image)
	}
	if spec.Resources == nil || spec.Resources.CPU != "2" || spec.Resources.Mem != "4Gi" {
		t.Errorf("spec.Resources = %+v, want {CPU:2 Mem:4Gi}", spec.Resources)
	}
}
