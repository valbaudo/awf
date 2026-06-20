package engine

import (
	"bytes"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestContainerSpecForImage(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Image: "alpine@sha256:abc123"},
		},
	}
	spec := ContainerSpecFor(wf, nil, "lab")
	if spec.Image != "alpine@sha256:abc123" {
		t.Errorf("spec.Image = %q, want \"alpine@sha256:abc123\"", spec.Image)
	}
	if spec.Compose != nil {
		t.Errorf("spec.Compose = %v, want nil (image-mode)", spec.Compose)
	}
	if spec.Resources != nil {
		t.Errorf("spec.Resources = %+v, want nil", spec.Resources)
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
	spec := ContainerSpecFor(wf, nil, "lab")
	if spec.Resources == nil || spec.Resources.CPU != "2" || spec.Resources.Mem != "4Gi" {
		t.Errorf("spec.Resources = %+v, want CPU=2 Mem=4Gi", spec.Resources)
	}
}

func TestContainerSpecForMissingContainer(t *testing.T) {
	wf := &ir.Workflow{Containers: map[string]ir.Container{}}
	spec := ContainerSpecFor(wf, nil, "ghost")
	if spec.Name != "ghost" {
		t.Errorf("spec.Name = %q, want \"ghost\"", spec.Name)
	}
	if spec.Image != "" {
		t.Errorf("spec.Image = %q, want empty", spec.Image)
	}
	if spec.Compose != nil {
		t.Errorf("spec.Compose = %v, want nil", spec.Compose)
	}
}

func TestContainerSpecForComposeBytesPropagate(t *testing.T) {
	composeBytes := []byte("services:\n  web:\n    image: nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000\n")
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Compose: "lab/compose.yml", Service: "web"},
		},
	}
	composeFiles := map[string][]byte{"lab/compose.yml": composeBytes}
	spec := ContainerSpecFor(wf, composeFiles, "lab")
	if !bytes.Equal(spec.Compose, composeBytes) {
		t.Errorf("spec.Compose != composeBytes")
	}
	if spec.ComposePath != "lab/compose.yml" {
		t.Errorf("spec.ComposePath = %q, want \"lab/compose.yml\"", spec.ComposePath)
	}
	if spec.Service != "web" {
		t.Errorf("spec.Service = %q, want \"web\"", spec.Service)
	}
	if spec.Image != "" {
		t.Errorf("spec.Image = %q, want empty (compose-mode)", spec.Image)
	}
}

// TestContainerSpecForComposeMissingBytesDefensive verifies the defensive
// branch in ContainerSpecFor — composeFiles map missing the declared key.
// The loader+validator invariant means this path is unreachable in
// production runs. The test exists so a future refactor can't silently
// regress the defensive behavior; it is NOT documenting an expected
// runtime scenario.
func TestContainerSpecForComposeMissingBytesDefensive(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Compose: "lab/compose.yml", Service: "web"},
		},
	}
	composeFiles := map[string][]byte{} // empty — defensive case
	spec := ContainerSpecFor(wf, composeFiles, "lab")
	if spec.Compose != nil {
		t.Errorf("spec.Compose = %v, want nil (no bytes available)", spec.Compose)
	}
	if spec.ComposePath != "lab/compose.yml" {
		t.Errorf("spec.ComposePath = %q, want propagated path", spec.ComposePath)
	}
}

// TestContainerSpecForCmdThreaded verifies that an image-mode Container's Cmd is
// propagated to ContainerSpec.Cmd.
func TestContainerSpecForCmdThreaded(t *testing.T) {
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Image: "alpine@sha256:abc123", Cmd: []string{"sleep", "infinity"}},
		},
	}
	spec := ContainerSpecFor(wf, nil, "lab")
	if len(spec.Cmd) != 2 || spec.Cmd[0] != "sleep" || spec.Cmd[1] != "infinity" {
		t.Errorf("spec.Cmd = %v, want [sleep infinity]", spec.Cmd)
	}
}

// TestContainerSpecForKeepaliveDisableKeepalive verifies the DisableKeepalive
// threading from ir.Container.Keepalive.
//
//   - keepalive absent (nil)  → DisableKeepalive false  (safe default: inject if needed)
//   - keepalive: true         → DisableKeepalive false  (explicit opt-in, same as default)
//   - keepalive: false        → DisableKeepalive true   (explicit opt-out)
func TestContainerSpecForKeepaliveDisableKeepalive(t *testing.T) {
	boolTrue := true
	boolFalse := false
	cases := []struct {
		name        string
		keepalive   *bool
		wantDisable bool
	}{
		{"absent", nil, false},
		{"explicit true", &boolTrue, false},
		{"explicit false", &boolFalse, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := &ir.Workflow{
				Containers: map[string]ir.Container{
					"lab": {Image: "alpine@sha256:abc123", Keepalive: c.keepalive},
				},
			}
			spec := ContainerSpecFor(wf, nil, "lab")
			if spec.DisableKeepalive != c.wantDisable {
				t.Errorf("keepalive=%v: DisableKeepalive = %v, want %v", c.keepalive, spec.DisableKeepalive, c.wantDisable)
			}
		})
	}
}
