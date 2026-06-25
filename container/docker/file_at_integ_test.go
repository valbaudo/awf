//go:build integ

package docker

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	cont "github.com/valbaudo/awf/container"
)

// TestFileAt_RoundTrip verifies WriteFileAt then ReadFileAt round-trips bytes
// inside a real container.
func TestFileAt_RoundTrip(t *testing.T) {
	cli, b := newTestBackend(t, "fileat-roundtrip")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "fileat-rt",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	want := []byte("hello from WriteFileAt\n")
	path := "/tmp/awf-test-roundtrip.txt"

	if err := b.WriteFileAt(ctx, h, path, want); err != nil {
		t.Fatalf("WriteFileAt: %v", err)
	}

	got, err := b.ReadFileAt(ctx, h, path)
	if err != nil {
		t.Fatalf("ReadFileAt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ReadFileAt returned %q, want %q", got, want)
	}
}

// TestFileAt_WriteOverwrites verifies that a second WriteFileAt replaces the
// first content (not appends).
func TestFileAt_WriteOverwrites(t *testing.T) {
	cli, b := newTestBackend(t, "fileat-overwrite")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "fileat-ow",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	path := "/tmp/awf-test-overwrite.txt"
	first := []byte("first write\n")
	second := []byte("second write — replaces first\n")

	if err := b.WriteFileAt(ctx, h, path, first); err != nil {
		t.Fatalf("WriteFileAt (first): %v", err)
	}
	if err := b.WriteFileAt(ctx, h, path, second); err != nil {
		t.Fatalf("WriteFileAt (second): %v", err)
	}

	got, err := b.ReadFileAt(ctx, h, path)
	if err != nil {
		t.Fatalf("ReadFileAt: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Errorf("ReadFileAt returned %q, want %q", got, second)
	}
}

// TestFileAt_ReadMissingPathIsError verifies that ReadFileAt of a non-existent
// path surfaces as a hard error, not a nil-content nil-error.
func TestFileAt_ReadMissingPathIsError(t *testing.T) {
	cli, b := newTestBackend(t, "fileat-missing")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "fileat-miss",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	_, err = b.ReadFileAt(ctx, h, "/no/such/path/awf-test.txt")
	if err == nil {
		t.Fatal("ReadFileAt: expected error for missing path, got nil")
	}
	t.Logf("ReadFileAt missing path error (expected): %v", err)
}

// TestFileAt_UnknownHandleIsError verifies that both ReadFileAt and WriteFileAt
// return a hard error for a handle that was never Created. No container needed.
func TestFileAt_UnknownHandleIsError(t *testing.T) {
	// newTestBackend is lightweight; no pullImage needed since we don't call Create.
	_, b := newTestBackend(t, "fileat-unknown")

	ctx := context.Background()
	bogus := cont.Handle{Name: "ghost", ID: fmt.Sprintf("nonexistent-%d", 999)}

	if _, err := b.ReadFileAt(ctx, bogus, "/etc/hostname"); err == nil {
		t.Error("ReadFileAt with unknown handle: expected error, got nil")
	}
	if err := b.WriteFileAt(ctx, bogus, "/etc/hostname", []byte("x")); err == nil {
		t.Error("WriteFileAt with unknown handle: expected error, got nil")
	}
}
