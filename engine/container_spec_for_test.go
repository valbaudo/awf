package engine

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestContainerSpecForImage(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Image: "alpine@sha256:abc123"},
		},
	}
	spec := containerSpecFor(wf, "lab")
	if spec.Image != "alpine@sha256:abc123" {
		t.Errorf("spec.Image = %q, want \"alpine@sha256:abc123\"", spec.Image)
	}
	if spec.Resources != nil {
		t.Errorf("spec.Resources = %+v, want nil (no resources declared)", spec.Resources)
	}
}

func TestContainerSpecForResources(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {
				Image:     "alpine@sha256:abc123",
				Resources: &ir.Resources{CPU: "2", Mem: "4Gi"},
			},
		},
	}
	spec := containerSpecFor(wf, "lab")
	if spec.Resources == nil || spec.Resources.CPU != "2" || spec.Resources.Mem != "4Gi" {
		t.Errorf("spec.Resources = %+v, want CPU=2 Mem=4Gi", spec.Resources)
	}
}

func TestContainerSpecForMissingContainer(t *testing.T) {
	// A name not in Containers (should be caught by validator first, but the
	// helper must not panic — it returns a minimal spec with just Name set).
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{},
	}
	spec := containerSpecFor(wf, "ghost")
	if spec.Name != "ghost" {
		t.Errorf("spec.Name = %q, want \"ghost\"", spec.Name)
	}
	if spec.Image != "" {
		t.Errorf("spec.Image = %q, want empty for missing container", spec.Image)
	}
}
