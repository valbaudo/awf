//go:build integ

package docker

import (
	"bytes"
	"context"
	"testing"

	cont "github.com/valbaudo/awf/container"
)

func TestBucket9c_CaptureFilesRoundTrip(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9c-roundtrip")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	// Write a file via Exec, read it back via CaptureFiles, assert byte-equal.
	want := []byte("hello captured world\n")
	if _, _, err := b.Exec(ctx, h, cont.Cmd{
		Run: `echo "hello captured world" > /tmp/awf-test.txt`,
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	files, err := b.CaptureFiles(ctx, h, []string{"/tmp/awf-test.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("CaptureFiles: len = %d, want 1", len(files))
	}
	if files[0].Path != "/tmp/awf-test.txt" {
		t.Errorf("Path = %q, want /tmp/awf-test.txt", files[0].Path)
	}
	if !bytes.Equal(files[0].Content, want) {
		t.Errorf("Content = %q, want %q", files[0].Content, want)
	}
}

func TestBucket9c_CaptureFilesMissingPathIsError(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9c-missing")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	if _, err := b.CaptureFiles(ctx, h, []string{"/never/written.txt"}); err == nil {
		t.Fatal("CaptureFiles missing path: err = nil, want non-nil")
	}
}

func TestBucket9c_CaptureFilesOrderingPreserved(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9c-order")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	// Write three files. Capture in a non-trivial order; assert the
	// CapturedFile slice preserves the request order.
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := b.Exec(ctx, h, cont.Cmd{Run: "echo " + name + " > /tmp/" + name + ".txt"}); err != nil {
			t.Fatalf("Exec write %s: %v", name, err)
		}
	}
	want := []string{"/tmp/c.txt", "/tmp/a.txt", "/tmp/b.txt"}
	files, err := b.CaptureFiles(ctx, h, want)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("CaptureFiles: len = %d, want 3", len(files))
	}
	for i, w := range want {
		if files[i].Path != w {
			t.Errorf("files[%d].Path = %q, want %q", i, files[i].Path, w)
		}
	}
}

// TestBucket9c_CaptureFilesPartialMissingErrors verifies that if even one
// path is missing, the whole call errors — no partial returns. Matches
// fake_test.go's TestFakeCaptureFilesMissingPath semantic.
func TestBucket9c_CaptureFilesPartialMissingErrors(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9c-partial")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	if _, _, err := b.Exec(ctx, h, cont.Cmd{Run: "echo present > /tmp/present.txt"}); err != nil {
		t.Fatalf("Exec write present: %v", err)
	}
	files, err := b.CaptureFiles(ctx, h, []string{"/tmp/present.txt", "/tmp/missing.txt"})
	if err == nil {
		t.Errorf("CaptureFiles partial-missing: err = nil, files = %+v, want non-nil err", files)
	}
	if files != nil {
		t.Errorf("CaptureFiles partial-missing: files = %+v, want nil (no partial returns)", files)
	}
}
