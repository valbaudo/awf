package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestCreateRejectsComposeSpec(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Create(context.Background(), container.ContainerSpec{
		Name:    "lab",
		Compose: []byte("services: {}"),
	})
	if err == nil {
		t.Fatal("err = nil, want non-nil (compose rejected)")
	}
	if !strings.Contains(err.Error(), "compose-mode not supported") {
		t.Errorf("err = %q, want substring 'compose-mode not supported'", err)
	}
	if !strings.Contains(err.Error(), "container/native:") {
		t.Errorf("err = %q, want prefix 'container/native:'", err)
	}
}

func TestCreateIgnoresImageAndMakesWorkdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b, err := native.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{
		Name:  "lab",
		Image: "oci://example/image@sha256:abc",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" {
		t.Errorf("Handle.Name = %q, want \"lab\"", h.Name)
	}
	want := filepath.Join(root, "lab")
	if h.ID != want {
		t.Errorf("Handle.ID = %q, want %q (workdir path)", h.ID, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("workdir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("workdir is not a directory")
	}
}

func TestCreateBareSpecAccepted(t *testing.T) {
	t.Parallel()
	// ContainerSpec{Name: "lab"} (no Image, no Compose) — passed by
	// backendtest.RunBasicContract. Native must accept and create a workdir.
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" || h.ID == "" {
		t.Errorf("Handle = %+v", h)
	}
}

func TestDestroyRemovesWorkdir(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(h.ID); err != nil {
		t.Fatalf("workdir missing pre-Destroy: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(h.ID); !os.IsNotExist(err) {
		t.Errorf("workdir still present after Destroy: err=%v", err)
	}
}

func TestDoubleDestroyErrors(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err == nil {
		t.Error("second Destroy returned nil, want error (handle gone)")
	}
}
