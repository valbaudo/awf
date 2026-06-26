package container

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

const testTreeDir = "/work/.awf/claude-session/RUN/projects"

// TestFakeWriteReadTreeAtRoundTrip verifies the complete round-trip:
// WriteTreeAt then ReadTreeAt returns a tar that ExtractTreeTar decodes to the
// original files map.
func TestFakeWriteReadTreeAtRoundTrip(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	h, err := f.Create(ctx, ContainerSpec{Name: "c"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := map[string][]byte{
		"a.jsonl":   []byte(`{"line":1}`),
		"sub/b.txt": []byte("hello"),
	}
	tarGz, err := BuildTreeTar(want)
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}

	if err := f.WriteTreeAt(ctx, h, testTreeDir, tarGz); err != nil {
		t.Fatalf("WriteTreeAt: %v", err)
	}

	got, err := f.ReadTreeAt(ctx, h, testTreeDir)
	if err != nil {
		t.Fatalf("ReadTreeAt: %v", err)
	}

	extracted, err := ExtractTreeTar(got, TreeTarMaxBytes, TreeTarMaxEntries)
	if err != nil {
		t.Fatalf("ExtractTreeTar after ReadTreeAt: %v", err)
	}
	if len(extracted) != len(want) {
		t.Fatalf("extracted file count: got %d, want %d; keys = %v", len(extracted), len(want), extracted)
	}
	for p, wantContent := range want {
		if !bytes.Equal(extracted[p], wantContent) {
			t.Errorf("path %q: got %q, want %q", p, extracted[p], wantContent)
		}
	}
}

// TestFakeReadTreeAtUnknownHandle verifies that ReadTreeAt returns an error for
// a handle ID that was never Created.
func TestFakeReadTreeAtUnknownHandle(t *testing.T) {
	f := NewFake()
	ghost := Handle{Name: "ghost", ID: "fake-99"}
	if _, err := f.ReadTreeAt(context.Background(), ghost, testTreeDir); err == nil {
		t.Fatal("ReadTreeAt with unknown handle: want error, got nil")
	}
}

// TestFakeWriteTreeAtUnknownHandle verifies that WriteTreeAt returns an error
// for a handle ID that was never Created.
func TestFakeWriteTreeAtUnknownHandle(t *testing.T) {
	f := NewFake()
	ghost := Handle{Name: "ghost", ID: "fake-99"}
	tarGz, _ := BuildTreeTar(map[string][]byte{"f": []byte("x")})
	if err := f.WriteTreeAt(context.Background(), ghost, testTreeDir, tarGz); err == nil {
		t.Fatal("WriteTreeAt with unknown handle: want error, got nil")
	}
}

// TestFakeReadTreeAtMissingDir verifies that ReadTreeAt returns an error when
// no files exist under the requested directory prefix.
func TestFakeReadTreeAtMissingDir(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	h, err := f.Create(ctx, ContainerSpec{Name: "c"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No files written — dir is absent.
	if _, err := f.ReadTreeAt(ctx, h, testTreeDir); err == nil {
		t.Fatal("ReadTreeAt of missing dir: want error, got nil")
	}
}

// TestFakeWriteReadTreeAtCtxCancel verifies that a cancelled context is
// propagated immediately.
func TestFakeWriteReadTreeAtCtxCancel(t *testing.T) {
	f := NewFake()
	h, _ := f.Create(context.Background(), ContainerSpec{Name: "c"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tarGz, _ := BuildTreeTar(map[string][]byte{"f": []byte("x")})
	if err := f.WriteTreeAt(ctx, h, testTreeDir, tarGz); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteTreeAt cancelled: err = %v, want context.Canceled", err)
	}
	if _, err := f.ReadTreeAt(ctx, h, testTreeDir); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadTreeAt cancelled: err = %v, want context.Canceled", err)
	}
}
