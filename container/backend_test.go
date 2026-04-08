package container

import (
	"errors"
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
	if ErrUnsupported == nil {
		t.Fatal("ErrUnsupported is nil")
	}
	if !errors.Is(ErrUnsupported, ErrUnsupported) {
		t.Errorf("errors.Is(ErrUnsupported, ErrUnsupported) = false")
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
	c2 := Caps{Snapshot: SnapshotNone}
	if c2.Snapshot != SnapshotNone {
		t.Errorf("Caps{Snapshot: SnapshotNone}.Snapshot = %q", c2.Snapshot)
	}
}
