package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobsPutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("hello, awf")
	ref, err := bs.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	// Ref shape: "awf-d1:sha256:<64-hex>".
	if !strings.HasPrefix(ref, "awf-d1:sha256:") {
		t.Errorf("ref %q missing expected prefix", ref)
	}
	expectedHash := sha256.Sum256(want)
	wantRef := "awf-d1:sha256:" + hex.EncodeToString(expectedHash[:])
	if ref != wantRef {
		t.Errorf("ref = %q, want %q", ref, wantRef)
	}
	got, err := bs.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("Get returned %q, want %q", got, want)
	}
}

func TestBlobsDedupSameContentSameRef(t *testing.T) {
	dir := t.TempDir()
	bs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte("dedup me")
	r1, err := bs.Put(b)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := bs.Put(b)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Errorf("duplicate Put returned different refs: %q vs %q", r1, r2)
	}
	// Exactly one on-disk file for this hash — no stale `.tmp.*` residue from the second Put.
	hash := sha256.Sum256(b)
	hashHex := hex.EncodeToString(hash[:])
	shard := filepath.Join(dir, "sha256", hashHex[:2])
	entries, err := os.ReadDir(shard)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != hashHex {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("shard %q contents = %v, want exactly [%s] (no .tmp residue)", shard, names, hashHex)
	}
}

func TestBlobsGetDetectsCorruption(t *testing.T) {
	// Put bytes, then mutate the underlying file outside the API. Get must surface
	// ErrCorruption (the ref's address no longer matches the file's content).
	dir := t.TempDir()
	bs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bs.Put([]byte("verify-me"))
	if err != nil {
		t.Fatal(err)
	}
	// Find the on-disk file and flip a byte.
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("malformed ref produced by Put: %q", ref)
	}
	hashHex := parts[2]
	path := filepath.Join(dir, "sha256", hashHex[:2], hashHex)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := bs.Get(ref); !errors.Is(err, ErrCorruption) {
		t.Errorf("Get on corrupted blob: err = %v, want ErrCorruption", err)
	}
}

func TestBlobsGetRejectsMalformedRef(t *testing.T) {
	dir := t.TempDir()
	bs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"",                                  // empty
		"sha256:" + strings.Repeat("a", 64), // missing awf-d1 prefix
		"awf-d1:sha256:tooShort",            // not 64 hex
		"awf-d1:sha256:" + strings.Repeat("a", 63),           // 63 hex
		"awf-d1:sha256:" + strings.Repeat("a", 65),           // 65 hex
		"awf-d1:sha256:" + strings.Repeat("g", 64),           // non-hex
		"awf-d1:sha256:" + strings.Repeat("A", 64),           // uppercase hex (RefFor emits lowercase)
		"awf-d1:blake3:" + strings.Repeat("a", 64),           // wrong algo (Phase 1 supports sha256 only)
		"awf-d1:sha256:" + strings.Repeat("a", 60) + "../..", // path-traversal attempt
	}
	for _, ref := range bad {
		if _, err := bs.Get(ref); !errors.Is(err, ErrBadRef) {
			t.Errorf("Get(%q): err = %v, want ErrBadRef", ref, err)
		}
	}
}

func TestBlobsGetMissingReturnsFsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	bs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Well-formed but never-put ref.
	ref := "awf-d1:sha256:" + strings.Repeat("d", 64)
	_, err = bs.Get(ref)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get on missing ref: err = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestBlobsInterfaceSatisfiedByFSBlobs(t *testing.T) {
	var _ Blobs = (*FSBlobs)(nil)
}
