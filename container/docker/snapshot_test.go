package docker

import (
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
// streaming pass (Task 4); this helper is for assertion convenience only.
func readDiffTar(r io.Reader) (adds map[string][]byte, syms map[string]string, dirs map[string]bool, deletes []string, err error) {
	adds, syms, dirs = map[string][]byte{}, map[string]string{}, map[string]bool{}
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer func() { _ = gr.Close() }()
	tr := newTarReaderForTest(gr)
	for {
		hdr, body, done, err := tr.next()
		if done {
			break
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		switch {
		case hdr.Name == deletesManifestPath:
			for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
				if line != "" {
					deletes = append(deletes, line)
				}
			}
		case hdr.Typeflag == typeflagSymlink:
			syms[hdr.Name] = hdr.Linkname
		case hdr.Typeflag == typeflagDir:
			dirs[hdr.Name] = true
		case hdr.Typeflag == typeflagReg:
			adds[hdr.Name] = body
		}
	}
	return
}
