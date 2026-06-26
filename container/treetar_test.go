package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// buildMaliciousTar crafts a gzip-tar containing a path-escape entry for
// ExtractTreeTar rejection tests. It bypasses BuildTreeTar deliberately so
// the malicious name reaches ExtractTreeTar unchanged.
func buildMaliciousTar(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Size:     int64(len(content)),
		Mode:     0o644,
	}); err != nil {
		t.Fatalf("buildMaliciousTar WriteHeader: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("buildMaliciousTar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildMaliciousTar Close tw: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("buildMaliciousTar Close gw: %v", err)
	}
	return buf.Bytes()
}

// TestBuildExtractTreeTarRoundTrip verifies that ExtractTreeTar(BuildTreeTar(files))
// returns a map identical to the original.
func TestBuildExtractTreeTarRoundTrip(t *testing.T) {
	files := map[string][]byte{
		"a.jsonl":   []byte(`{"event":"start"}`),
		"b/c.jsonl": []byte(`{"event":"end"}`),
	}
	tarGz, err := BuildTreeTar(files)
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}
	got, err := ExtractTreeTar(tarGz, TreeTarMaxBytes, TreeTarMaxEntries)
	if err != nil {
		t.Fatalf("ExtractTreeTar: %v", err)
	}
	if len(got) != len(files) {
		t.Fatalf("file count: got %d, want %d; files = %v", len(got), len(files), got)
	}
	for p, want := range files {
		if !bytes.Equal(got[p], want) {
			t.Errorf("path %q: got %q, want %q", p, got[p], want)
		}
	}
}

// TestBuildTreeTarDeterministic verifies that identical input always produces
// identical bytes — the content-hash invariant requires it.
func TestBuildTreeTarDeterministic(t *testing.T) {
	files := map[string][]byte{
		"a.jsonl":   []byte("aaa"),
		"b/c.jsonl": []byte("bbb"),
		"z.jsonl":   []byte("zzz"),
	}
	t1, err := BuildTreeTar(files)
	if err != nil {
		t.Fatalf("first BuildTreeTar: %v", err)
	}
	t2, err := BuildTreeTar(files)
	if err != nil {
		t.Fatalf("second BuildTreeTar: %v", err)
	}
	if !bytes.Equal(t1, t2) {
		t.Error("BuildTreeTar is not deterministic: same input yielded different bytes")
	}
}

// TestExtractTreeTarRejectsDotDot verifies that a tar entry with a path-escape
// name (../<anything>) is rejected.
func TestExtractTreeTarRejectsDotDot(t *testing.T) {
	bad := buildMaliciousTar(t, "../escape", []byte("payload"))
	if _, err := ExtractTreeTar(bad, TreeTarMaxBytes, TreeTarMaxEntries); err == nil {
		t.Fatal("ExtractTreeTar with ../escape entry: want error, got nil")
	}
}

// TestExtractTreeTarRejectsAbsolute verifies that an absolute-path entry in the
// tar is rejected.
func TestExtractTreeTarRejectsAbsolute(t *testing.T) {
	bad := buildMaliciousTar(t, "/etc/passwd", []byte("root:x:0:0"))
	if _, err := ExtractTreeTar(bad, TreeTarMaxBytes, TreeTarMaxEntries); err == nil {
		t.Fatal("ExtractTreeTar with /etc/passwd entry: want error, got nil")
	}
}

// TestExtractTreeTarEnforcesBytesCap verifies that ExtractTreeTar returns an
// error when the decompressed byte total exceeds maxBytes.
func TestExtractTreeTarEnforcesBytesCap(t *testing.T) {
	files := map[string][]byte{
		"big.bin": bytes.Repeat([]byte("x"), 1024),
	}
	tarGz, err := BuildTreeTar(files)
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}
	// Cap at 100 bytes — far below the 1024-byte file.
	if _, err := ExtractTreeTar(tarGz, 100, TreeTarMaxEntries); err == nil {
		t.Fatal("ExtractTreeTar over byte cap: want error, got nil")
	}
}

// TestExtractTreeTarEnforcesEntryCap verifies that ExtractTreeTar returns an
// error when the entry count exceeds maxEntries.
func TestExtractTreeTarEnforcesEntryCap(t *testing.T) {
	files := map[string][]byte{
		"f1.txt": []byte("a"),
		"f2.txt": []byte("b"),
		"f3.txt": []byte("c"),
	}
	tarGz, err := BuildTreeTar(files)
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}
	// Cap at 2 entries — the tar has at least 3 regular files.
	if _, err := ExtractTreeTar(tarGz, TreeTarMaxBytes, 2); err == nil {
		t.Fatal("ExtractTreeTar over entry cap: want error, got nil")
	}
}
