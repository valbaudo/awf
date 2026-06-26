//go:build integ

package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"testing"

	cont "github.com/valbaudo/awf/container"
)

// TestTreeAt_RoundTrip writes a projects/-style tree into a real container via
// WriteTreeAt, reads it back with ReadTreeAt, and verifies:
//   - returned gzip-tar is decodable by ExtractTreeTar
//   - all file content round-trips byte-for-byte
//   - no entry has a "projects/" prefix (entries are relative-to-dir)
func TestTreeAt_RoundTrip(t *testing.T) {
	cli, b := newTestBackend(t, "treeat-roundtrip")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "treeat-rt",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	// Build a test tree that resembles a claude-code projects/ subtree.
	wantFiles := map[string][]byte{
		"config/settings.json": []byte(`{"session":"test-session-id"}`),
		"cache/history.jsonl":  []byte("line1\nline2\nline3\n"),
		"cache/sub/nested.txt": []byte("nested content"),
	}
	tarGz, err := cont.BuildTreeTar(wantFiles)
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}

	dir := "/work/.awf/claude-session/RUN/projects"

	// WriteTreeAt: inject the tree into the container.
	if err := b.WriteTreeAt(ctx, h, dir, tarGz); err != nil {
		t.Fatalf("WriteTreeAt: %v", err)
	}

	// ReadTreeAt: capture the tree back.
	gotTarGz, err := b.ReadTreeAt(ctx, h, dir)
	if err != nil {
		t.Fatalf("ReadTreeAt: %v", err)
	}

	// Verify entries are relative-to-dir: no "projects/" prefix may appear.
	if err := checkNoLeadingComponent(t, gotTarGz, "projects"); err != nil {
		t.Errorf("ReadTreeAt returned entries with leading basename prefix: %v", err)
	}

	// Verify all file content survived the round-trip.
	gotFiles, err := cont.ExtractTreeTar(gotTarGz, cont.TreeTarMaxBytes, cont.TreeTarMaxEntries)
	if err != nil {
		t.Fatalf("ExtractTreeTar: %v", err)
	}
	for rel, want := range wantFiles {
		got, ok := gotFiles[rel]
		if !ok {
			t.Errorf("round-trip: missing entry %q", rel)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("round-trip: %q: got %q, want %q", rel, got, want)
		}
	}
	// No extra files should appear beyond what we wrote.
	for rel := range gotFiles {
		if _, ok := wantFiles[rel]; !ok {
			t.Errorf("round-trip: unexpected extra entry %q", rel)
		}
	}
}

// TestTreeAt_WriteOverwrites verifies that a second WriteTreeAt replaces the
// previous content (not merges or appends).
func TestTreeAt_WriteOverwrites(t *testing.T) {
	cli, b := newTestBackend(t, "treeat-overwrite")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "treeat-ow",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	dir := "/tmp/awf-tree-ow"

	first := map[string][]byte{"a.txt": []byte("first")}
	firstTar, err := cont.BuildTreeTar(first)
	if err != nil {
		t.Fatalf("BuildTreeTar (first): %v", err)
	}
	if err := b.WriteTreeAt(ctx, h, dir, firstTar); err != nil {
		t.Fatalf("WriteTreeAt (first): %v", err)
	}

	second := map[string][]byte{"a.txt": []byte("second — overwrites first")}
	secondTar, err := cont.BuildTreeTar(second)
	if err != nil {
		t.Fatalf("BuildTreeTar (second): %v", err)
	}
	if err := b.WriteTreeAt(ctx, h, dir, secondTar); err != nil {
		t.Fatalf("WriteTreeAt (second): %v", err)
	}

	gotTarGz, err := b.ReadTreeAt(ctx, h, dir)
	if err != nil {
		t.Fatalf("ReadTreeAt: %v", err)
	}
	gotFiles, err := cont.ExtractTreeTar(gotTarGz, cont.TreeTarMaxBytes, cont.TreeTarMaxEntries)
	if err != nil {
		t.Fatalf("ExtractTreeTar: %v", err)
	}
	if got, ok := gotFiles["a.txt"]; !ok {
		t.Error("a.txt missing")
	} else if !bytes.Equal(got, second["a.txt"]) {
		t.Errorf("a.txt: got %q, want %q", got, second["a.txt"])
	}
}

// TestTreeAt_ReadMissingDirIsError verifies that ReadTreeAt of a non-existent
// directory returns a hard error, not a nil result.
func TestTreeAt_ReadMissingDirIsError(t *testing.T) {
	cli, b := newTestBackend(t, "treeat-missing")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "treeat-miss",
		Image: alpineDigest,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	_, err = b.ReadTreeAt(ctx, h, "/no/such/directory/awf-test")
	if err == nil {
		t.Fatal("ReadTreeAt: expected error for missing directory, got nil")
	}
	t.Logf("ReadTreeAt missing dir error (expected): %v", err)
}

// TestTreeAt_UnknownHandleIsError verifies both methods return a hard error
// for a handle that was never Created.
func TestTreeAt_UnknownHandleIsError(t *testing.T) {
	_, b := newTestBackend(t, "treeat-unknown")

	ctx := context.Background()
	bogus := cont.Handle{Name: "ghost", ID: fmt.Sprintf("nonexistent-%d", 999)}

	tarGz, err := cont.BuildTreeTar(map[string][]byte{"x.txt": []byte("x")})
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}

	if _, err := b.ReadTreeAt(ctx, bogus, "/tmp/test"); err == nil {
		t.Error("ReadTreeAt with unknown handle: expected error, got nil")
	}
	if err := b.WriteTreeAt(ctx, bogus, "/tmp/test", tarGz); err == nil {
		t.Error("WriteTreeAt with unknown handle: expected error, got nil")
	}
}

// checkNoLeadingComponent returns an error if any entry name in the gzip-tar
// starts with component+"/" or equals component. This verifies that
// ReadTreeAt strips the Docker-added basename prefix from all entries.
func checkNoLeadingComponent(t *testing.T, tarGz []byte, component string) error {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		name := hdr.Name
		if name == component || name == component+"/" {
			return fmt.Errorf("entry %q equals the basename component (prefix not stripped)", name)
		}
		if len(name) > len(component)+1 &&
			name[:len(component)+1] == component+"/" {
			return fmt.Errorf("entry %q still has %q/ prefix (not stripped)", name, component)
		}
	}
	return nil
}
