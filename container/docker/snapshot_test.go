package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	cont "github.com/valbaudo/awf/container"
)

func TestFormatAndParseSnapshotRef(t *testing.T) {
	cases := []struct {
		name    string
		blobRef string
		image   string
		cmd     snapshotCmdSpec
	}{
		{
			name:    "image-with-digest-and-cmd",
			blobRef: "awf-d1:sha256:abc123",
			image:   "alpine@sha256:d9e853e",
			cmd:     snapshotCmdSpec{Cmd: []string{"sleep", "infinity"}},
		},
		{
			name:    "registry-prefixed-image-no-cmd",
			blobRef: "awf-d1:sha256:0000000000000000000000000000000000000000000000000000000000000000",
			image:   "oci://registry.example.com/runner@sha256:abc",
			cmd:     snapshotCmdSpec{},
		},
		{
			name:    "cmd-and-entrypoint",
			blobRef: "awf-d1:sha256:ff",
			image:   "myreg/myimage@sha256:def",
			cmd:     snapshotCmdSpec{Cmd: []string{"-c", "echo hi"}, Entrypoint: []string{"/bin/sh"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, err := formatSnapshotRef(c.blobRef, c.image, c.cmd)
			if err != nil {
				t.Fatalf("formatSnapshotRef: %v", err)
			}
			gotBlob, gotImg, gotCmd, err := parseSnapshotRef(ref)
			if err != nil {
				t.Fatalf("parseSnapshotRef(%q): %v", ref, err)
			}
			if gotBlob != c.blobRef || gotImg != c.image {
				t.Errorf("blob/image roundtrip: got (%q, %q), want (%q, %q)", gotBlob, gotImg, c.blobRef, c.image)
			}
			// With omitempty JSON tags, nil and empty slices both unmarshal
			// to nil; reflect.DeepEqual is sufficient (no custom helper needed).
			wantCmd := snapshotCmdSpec{Cmd: nilIfEmpty(c.cmd.Cmd), Entrypoint: nilIfEmpty(c.cmd.Entrypoint)}
			if !reflect.DeepEqual(gotCmd, wantCmd) {
				t.Errorf("cmd roundtrip: got %+v, want %+v", gotCmd, wantCmd)
			}
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestParseSnapshotRefRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"awf-d1:sha256:abc",
		"awf-d1:sha256:abc@only-one-at",
		"@alpine@e30=",
		"awf-d1:sha256:abc@@e30=",
		"awf-d1:sha256:abc@alpine@",
		"wrong-prefix:abc@alpine@e30=",
		"awf-d1:sha256:abc@alpine@not-base64!!!",
		"awf-d1:sha256:abc@alpine@bm90LWpzb24=",
	}
	for _, c := range cases {
		if _, _, _, err := parseSnapshotRef(cont.SnapshotRef(c)); err == nil {
			t.Errorf("parseSnapshotRef(%q): err = nil, want non-nil", c)
		}
	}
}

func TestDiffTarWriter_RegularFile_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 1<<20)
	if err := dw.WriteRegular("/work/a.txt", bytes.NewReader([]byte("hello\n")), 6); err != nil {
		t.Fatalf("WriteRegular a: %v", err)
	}
	if err := dw.WriteRegular("/work/b.txt", bytes.NewReader([]byte("world\n")), 6); err != nil {
		t.Fatalf("WriteRegular b: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	adds, syms, dirs, deletes, err := readDiffTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readDiffTar: %v", err)
	}
	if !bytes.Equal(adds["/work/a.txt"], []byte("hello\n")) {
		t.Errorf("adds[/work/a.txt] = %q, want %q", adds["/work/a.txt"], "hello\n")
	}
	if !bytes.Equal(adds["/work/b.txt"], []byte("world\n")) {
		t.Errorf("adds[/work/b.txt] = %q, want %q", adds["/work/b.txt"], "world\n")
	}
	if len(syms) != 0 || len(dirs) != 0 || len(deletes) != 0 {
		t.Errorf("expected only regulars; got syms=%v dirs=%v deletes=%v", syms, dirs, deletes)
	}
}

func TestDiffTarWriter_Symlink(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 1<<20)
	if err := dw.WriteSymlink("/work/link", "/work/target"); err != nil {
		t.Fatalf("WriteSymlink: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, syms, _, _, err := readDiffTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readDiffTar: %v", err)
	}
	if syms["/work/link"] != "/work/target" {
		t.Errorf("symlink target = %q, want /work/target", syms["/work/link"])
	}
}

func TestDiffTarWriter_EmptyDirectory(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 1<<20)
	if err := dw.WriteDir("/work/empty", 0o755); err != nil {
		t.Fatalf("WriteDir: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, dirs, _, err := readDiffTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readDiffTar: %v", err)
	}
	if !dirs["/work/empty"] {
		t.Errorf("dirs = %v, want /work/empty present", dirs)
	}
}

func TestDiffTarWriter_DeletesManifest(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 1<<20)
	if err := dw.WriteDeletes([]string{"/old/a", "/old/b"}); err != nil {
		t.Fatalf("WriteDeletes: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, _, deletes, err := readDiffTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readDiffTar: %v", err)
	}
	if len(deletes) != 2 || deletes[0] != "/old/a" || deletes[1] != "/old/b" {
		t.Errorf("deletes = %v, want [/old/a /old/b]", deletes)
	}
}

func TestDiffTarWriter_NoDeletes_OmitsManifest(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 1<<20)
	if err := dw.WriteRegular("/x", bytes.NewReader([]byte("y")), 1); err != nil {
		t.Fatalf("WriteRegular: %v", err)
	}
	if err := dw.WriteDeletes(nil); err != nil {
		t.Fatalf("WriteDeletes(nil): %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, _, deletes, err := readDiffTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readDiffTar: %v", err)
	}
	if len(deletes) != 0 {
		t.Errorf("expected zero deletes, got %v", deletes)
	}
}

func TestDiffTarWriter_CapExceededReturnsTypedError(t *testing.T) {
	var buf bytes.Buffer
	dw := newDiffTarWriter(&buf, 100)
	payload := bytes.Repeat([]byte("a"), 4096)
	err := dw.WriteRegular("/work/big.txt", bytes.NewReader(payload), int64(len(payload)))
	if err == nil {
		err = dw.Close()
	}
	if err == nil {
		t.Fatal("expected ErrSnapshotTooLarge, got nil")
	}
	var typed *ErrSnapshotTooLarge
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want errors.As(_, *ErrSnapshotTooLarge)", err)
	}
	if typed.Limit != 100 {
		t.Errorf("ErrSnapshotTooLarge.Limit = %d, want 100", typed.Limit)
	}
	if !strings.Contains(typed.Path, "big.txt") {
		t.Errorf("ErrSnapshotTooLarge.Path = %q, want to contain \"big.txt\"", typed.Path)
	}
}

// readDiffTar is a TEST-ONLY helper that materializes the diff into shape
// maps for round-trip verification. Production Restore uses an io.Pipe
// streaming pass; this helper is for assertion convenience only.
func readDiffTar(r io.Reader) (adds map[string][]byte, syms map[string]string, dirs map[string]bool, deletes []string, err error) {
	adds, syms, dirs = map[string][]byte{}, map[string]string{}, map[string]bool{}
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		var body []byte
		if hdr.Size > 0 {
			body, err = io.ReadAll(tr)
			if err != nil {
				return nil, nil, nil, nil, err
			}
		}
		switch {
		case hdr.Name == deletesManifestPath:
			for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
				if line != "" {
					deletes = append(deletes, line)
				}
			}
		case hdr.Typeflag == tar.TypeSymlink:
			syms[hdr.Name] = hdr.Linkname
		case hdr.Typeflag == tar.TypeDir:
			dirs[hdr.Name] = true
		case hdr.Typeflag == tar.TypeReg:
			adds[hdr.Name] = body
		}
	}
	return
}

func TestShellQuotePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/work/a.txt", "'/work/a.txt'"},
		{"-rf", "'-rf'"},
		{"/tmp/with space.txt", "'/tmp/with space.txt'"},
		{"with'quote", `'with'\''quote'`},
		{"two''quotes", `'two'\'''\''quotes'`},
		{"", "''"},
	}
	for _, c := range cases {
		got := shellQuotePath(c.in)
		if got != c.want {
			t.Errorf("shellQuotePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStreamPlainTarFromDiff_RoundTrip(t *testing.T) {
	// Build a diff-tar with: 1 regular file, 1 symlink, 1 dir, 2 deletes.
	var diff bytes.Buffer
	dw := newDiffTarWriter(&diff, 1<<20)
	if err := dw.WriteRegular("/work/a.txt", bytes.NewReader([]byte("hello")), 5); err != nil {
		t.Fatalf("WriteRegular: %v", err)
	}
	if err := dw.WriteSymlink("/work/link", "/work/target"); err != nil {
		t.Fatalf("WriteSymlink: %v", err)
	}
	if err := dw.WriteDir("/work/dir", 0o755); err != nil {
		t.Fatalf("WriteDir: %v", err)
	}
	if err := dw.WriteDeletes([]string{"/old/a", "/old/b"}); err != nil {
		t.Fatalf("WriteDeletes: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Stream into a plain tar; verify the deletes return value + the tar contents.
	var plain bytes.Buffer
	deletes, err := streamPlainTarFromDiff(&plain, diff.Bytes())
	if err != nil {
		t.Fatalf("streamPlainTarFromDiff: %v", err)
	}
	if len(deletes) != 2 || deletes[0] != "/old/a" || deletes[1] != "/old/b" {
		t.Errorf("deletes = %v, want [/old/a /old/b]", deletes)
	}

	// Re-read the plain tar; verify entries.
	tr := tar.NewReader(&plain)
	seen := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Name == deletesManifestPath {
			t.Errorf("plain tar contains %q sidecar; expected it stripped", deletesManifestPath)
		}
		if strings.HasPrefix(hdr.Name, "/") {
			t.Errorf("plain tar entry name %q has leading slash; expected stripped", hdr.Name)
		}
		seen[hdr.Name] = hdr
		if hdr.Size > 0 {
			if _, err := io.ReadAll(tr); err != nil {
				t.Fatalf("read body of %q: %v", hdr.Name, err)
			}
		}
	}
	if seen["work/a.txt"] == nil {
		t.Errorf("missing work/a.txt entry")
	}
	if h := seen["work/link"]; h == nil || h.Linkname != "/work/target" {
		t.Errorf("symlink entry wrong: %+v", h)
	}
	if h := seen["work/dir"]; h == nil || h.Typeflag != tar.TypeDir {
		t.Errorf("dir entry wrong: %+v", h)
	}
}

func TestStreamPlainTarFromDiff_NoDeletes(t *testing.T) {
	var diff bytes.Buffer
	dw := newDiffTarWriter(&diff, 1<<20)
	if err := dw.WriteRegular("/x", bytes.NewReader([]byte("y")), 1); err != nil {
		t.Fatalf("WriteRegular: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var plain bytes.Buffer
	deletes, err := streamPlainTarFromDiff(&plain, diff.Bytes())
	if err != nil {
		t.Fatalf("streamPlainTarFromDiff: %v", err)
	}
	if len(deletes) != 0 {
		t.Errorf("deletes = %v, want []", deletes)
	}
}

func TestStreamPlainTarFromDiff_RejectsCorruptedGzip(t *testing.T) {
	_, err := streamPlainTarFromDiff(io.Discard, []byte("not gzipped"))
	if err == nil {
		t.Fatal("streamPlainTarFromDiff with corrupt input: err = nil, want non-nil")
	}
}
