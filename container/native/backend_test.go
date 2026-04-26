package native_test

import (
	"os"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

func TestNewBootstrapsTmpAwf(t *testing.T) {
	t.Parallel()
	// native.New must create /tmp/awf/ (host) so the dispatcher's
	// AWF_OUTPUT path (/tmp/awf/<step>.json) is writable by the user's
	// step on first run. Decision 5 + N11 in the spec.
	_, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat("/tmp/awf")
	if err != nil {
		t.Fatalf("/tmp/awf should exist after New: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("/tmp/awf is not a directory")
	}
}

func TestNewRejectsEmptyWorkdirRoot(t *testing.T) {
	t.Parallel()
	_, err := native.New("")
	if err == nil {
		t.Fatal("err = nil, want non-nil for empty workdirRoot")
	}
}

func TestCapabilitiesReturnsSnapshotNone(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotNone", got)
	}
}
