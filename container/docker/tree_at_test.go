package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

// buildPlainTar builds an uncompressed tar from the given (name, typeflag,
// body) triples. Helper for unit-testing rerootTarEntries without Docker.
func buildPlainTar(t *testing.T, entries []tarTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := int64(0o644)
		if e.typ == tar.TypeDir {
			mode = 0o755
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     mode,
			Size:     int64(len(e.body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("buildPlainTar: WriteHeader %q: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("buildPlainTar: Write %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildPlainTar: Close: %v", err)
	}
	return buf.Bytes()
}

// readPlainTarNames extracts all entry names from an uncompressed tar.
func readPlainTarNames(t *testing.T, in []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(in))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("readPlainTarNames: Next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

// readPlainTarFiles extracts regular-file entry names and bodies from an
// uncompressed tar.
func readPlainTarFiles(t *testing.T, in []byte) map[string][]byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(in))
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("readPlainTarFiles: Next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("readPlainTarFiles: ReadAll %q: %v", hdr.Name, err)
		}
		out[hdr.Name] = body
	}
	return out
}

type tarTestEntry struct {
	name string
	typ  byte
	body []byte
}

// TestRerootTarEntries_StripPrefix verifies that the stripPrefix is removed
// from all entry names and entries lacking the prefix are silently dropped.
func TestRerootTarEntries_StripPrefix(t *testing.T) {
	in := buildPlainTar(t, []tarTestEntry{
		{name: "projects/", typ: tar.TypeDir},        // root sentinel — no prefix match after strip
		{name: "projects/config/", typ: tar.TypeDir}, // dir entry with prefix
		{name: "projects/config/settings.json", typ: tar.TypeReg, body: []byte(`{}`)},
		{name: "projects/data.bin", typ: tar.TypeReg, body: []byte("bin")},
		{name: "unrelated-entry", typ: tar.TypeReg, body: []byte("x")}, // lacks prefix → dropped
	})

	got, err := rerootTarEntries(in, "projects/", "")
	if err != nil {
		t.Fatalf("rerootTarEntries: %v", err)
	}

	names := readPlainTarNames(t, got)

	// "projects/" (root sentinel) and "unrelated-entry" are both dropped.
	want := map[string]bool{"config/": true, "config/settings.json": true, "data.bin": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected entry %q", n)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("missing expected entry %q", n)
	}
}

// TestRerootTarEntries_AddPrefix verifies that addPrefix is prepended to
// every entry name when stripPrefix is empty.
func TestRerootTarEntries_AddPrefix(t *testing.T) {
	in := buildPlainTar(t, []tarTestEntry{
		{name: "config/", typ: tar.TypeDir},
		{name: "config/settings.json", typ: tar.TypeReg, body: []byte(`{"k":"v"}`)},
	})

	addPrefix := "work/.awf/claude-session/RUN/projects/"
	got, err := rerootTarEntries(in, "", addPrefix)
	if err != nil {
		t.Fatalf("rerootTarEntries: %v", err)
	}

	names := readPlainTarNames(t, got)
	want := map[string]bool{
		addPrefix + "config/":              true,
		addPrefix + "config/settings.json": true,
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected entry %q", n)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("missing expected entry %q", n)
	}
}

// TestRerootTarEntries_RoundTrip verifies that stripping then re-adding the
// same prefix is a no-op for file names and content.
func TestRerootTarEntries_RoundTrip(t *testing.T) {
	prefix := "projects/"
	files := map[string][]byte{
		prefix + "a.txt":     []byte("hello"),
		prefix + "sub/b.txt": []byte("world"),
	}
	var entries []tarTestEntry
	// dir entries first
	entries = append(entries,
		tarTestEntry{name: prefix, typ: tar.TypeDir},
		tarTestEntry{name: prefix + "sub/", typ: tar.TypeDir},
	)
	for name, body := range files {
		entries = append(entries, tarTestEntry{name: name, typ: tar.TypeReg, body: body})
	}
	in := buildPlainTar(t, entries)

	// Strip "projects/", then add "projects/" back.
	stripped, err := rerootTarEntries(in, prefix, "")
	if err != nil {
		t.Fatalf("rerootTarEntries strip: %v", err)
	}
	readded, err := rerootTarEntries(stripped, "", prefix)
	if err != nil {
		t.Fatalf("rerootTarEntries add: %v", err)
	}

	// Content must survive the round-trip.
	gotFiles := readPlainTarFiles(t, readded)
	for name, want := range files {
		got, ok := gotFiles[name]
		if !ok {
			t.Errorf("round-trip: missing %q", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("round-trip: %q: got %q, want %q", name, got, want)
		}
	}
}

// TestRerootTarEntries_EscapeRejected verifies that an entry whose cleaned
// path escapes the root via ".." is rejected.
func TestRerootTarEntries_EscapeRejected(t *testing.T) {
	cases := []struct {
		name    string
		rawName string
	}{
		{"dotdot prefix", "../escape/file.txt"},
		{"dotdot plain", ".."},
		{"absolute", "/etc/passwd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := buildPlainTar(t, []tarTestEntry{
				{name: tc.rawName, typ: tar.TypeReg, body: []byte("x")},
			})
			_, err := rerootTarEntries(in, "", "prefix/")
			if err == nil {
				t.Errorf("rerootTarEntries: expected error for unsafe path %q, got nil", tc.rawName)
			}
		})
	}
}

// TestRerootTarEntries_RootSentinelDropped verifies that the root-dir sentinel
// entry Docker emits for CopyFromContainer is silently dropped rather than
// producing an empty-named entry.
func TestRerootTarEntries_RootSentinelDropped(t *testing.T) {
	in := buildPlainTar(t, []tarTestEntry{
		{name: "projects/", typ: tar.TypeDir}, // root sentinel
		{name: "projects/foo.txt", typ: tar.TypeReg, body: []byte("data")},
	})

	got, err := rerootTarEntries(in, "projects/", "")
	if err != nil {
		t.Fatalf("rerootTarEntries: %v", err)
	}

	for _, n := range readPlainTarNames(t, got) {
		if n == "" || n == "." || n == "/" {
			t.Errorf("root sentinel leaked as entry %q", n)
		}
	}

	files := readPlainTarFiles(t, got)
	if body, ok := files["foo.txt"]; !ok {
		t.Error("foo.txt missing after strip")
	} else if string(body) != "data" {
		t.Errorf("foo.txt body: got %q, want %q", body, "data")
	}
}

// TestRerootTarEntries_FileContentPreserved verifies that regular-file body
// bytes survive rerootTarEntries unchanged.
func TestRerootTarEntries_FileContentPreserved(t *testing.T) {
	content := []byte("the quick brown fox")
	in := buildPlainTar(t, []tarTestEntry{
		{name: "root/file.txt", typ: tar.TypeReg, body: content},
	})

	got, err := rerootTarEntries(in, "root/", "new/")
	if err != nil {
		t.Fatalf("rerootTarEntries: %v", err)
	}

	files := readPlainTarFiles(t, got)
	body, ok := files["new/file.txt"]
	if !ok {
		t.Fatal("new/file.txt missing in output")
	}
	if !bytes.Equal(body, content) {
		t.Errorf("body: got %q, want %q", body, content)
	}
}
