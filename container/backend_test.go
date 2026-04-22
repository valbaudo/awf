package container

import (
	"errors"
	"fmt"
	"testing"
)

func TestSnapshotModeConstants(t *testing.T) {
	// Pin the wire-shape strings — these are what Capabilities() advertises and what
	// OTel attrs will project. Renaming any of these would invalidate every existing
	// trace and Phase 4's Docker impl's contract.
	cases := map[SnapshotMode]string{
		SnapshotNone:  "none",
		SnapshotFSCoW: "fs-cow",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("SnapshotMode constant = %q, want %q", got, want)
		}
	}
}

func TestErrUnsupportedIsSentinel(t *testing.T) {
	// Wrapped sentinel still matches via errors.Is — this is the pattern
	// callers use ("container/fake: Snapshot: %w", ErrUnsupported), so the
	// match must hold through %w wrapping.
	wrapped := fmt.Errorf("container/fake: Snapshot: %w", ErrUnsupported)
	if !errors.Is(wrapped, ErrUnsupported) {
		t.Errorf("errors.Is(wrapped, ErrUnsupported) = false; want true")
	}
	// An unrelated error must NOT match (defends against sentinel aliasing).
	unrelated := errors.New("snapshot in Phase 2 fake")
	if errors.Is(unrelated, ErrUnsupported) {
		t.Errorf("errors.Is(unrelated, ErrUnsupported) = true (sentinel collision)")
	}
}

func TestCapsZeroValue(t *testing.T) {
	// Zero-value Caps has Snapshot = "" (empty), NOT SnapshotNone. Intentional —
	// backends MUST explicitly set Capabilities so a zero-value never silently
	// advertises "no snapshot." The fake's Capabilities() returns {SnapshotNone}
	// explicitly.
	var c Caps
	if c.Snapshot != "" {
		t.Errorf("zero-value Caps.Snapshot = %q, want empty string (forces explicit set)", c.Snapshot)
	}
}

func TestContainerSpecImageField(t *testing.T) {
	spec := ContainerSpec{
		Name:  "lab",
		Image: "alpine@sha256:abc123",
	}
	if spec.Image != "alpine@sha256:abc123" {
		t.Errorf("spec.Image = %q, want \"alpine@sha256:abc123\"", spec.Image)
	}
}

func TestContainerSpecResourcesField(t *testing.T) {
	spec := ContainerSpec{
		Name:      "lab",
		Image:     "alpine@sha256:abc123",
		Resources: &ContainerResources{CPU: "2", Mem: "4Gi"},
	}
	if spec.Resources == nil {
		t.Fatal("spec.Resources is nil")
	}
	if spec.Resources.CPU != "2" || spec.Resources.Mem != "4Gi" {
		t.Errorf("spec.Resources = %+v", spec.Resources)
	}
}

func TestContainerSpecResourcesNilByDefault(t *testing.T) {
	spec := ContainerSpec{Name: "lab"}
	if spec.Resources != nil {
		t.Errorf("default Resources = %+v, want nil", spec.Resources)
	}
}
