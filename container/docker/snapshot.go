package docker

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/valbaudo/awf/container"
)

// deletesManifestPath is the fixed in-tar path of the deletes sidecar file.
const deletesManifestPath = ".awf-deletes"

const (
	typeflagReg     = tar.TypeReg
	typeflagDir     = tar.TypeDir
	typeflagSymlink = tar.TypeSymlink
)

// ErrSnapshotTooLarge is returned by Snapshot when the gzip-compressed
// diff-tar exceeds the Backend's snapshotMaxBlobBytes cap (default 256 MiB;
// override via WithSnapshotMaxBlobBytes). The engine (slice 4.5+ wiring)
// propagates it as permanent_failure (Phase 4 design decision 11).
//
// Path is the tar entry being written when the cap tripped. Size is the
// cumulative gzip output bytes at the moment the cap tripped (>= Limit).
// Limit is the Backend's configured cap.
type ErrSnapshotTooLarge struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *ErrSnapshotTooLarge) Error() string {
	return fmt.Sprintf("container/docker: snapshot exceeded size cap: writing %q pushed compressed total to %d bytes (cap %d bytes); set WithSnapshotMaxBlobBytes to raise", e.Path, e.Size, e.Limit)
}

// snapshotCmdSpec captures the effective Cmd/Entrypoint of a container at
// Snapshot time, serialized into the SnapshotRef so Restore can re-create
// the container with the same runtime configuration. omitempty JSON tags
// make nil and empty slices round-trip symmetrically (both encode to no
// field; unmarshal restores both as nil — Cmd interpretation: nil means
// image default applies, same as empty, so the lossy nil/empty conflation
// is semantically correct).
type snapshotCmdSpec struct {
	Cmd        []string `json:"cmd,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
}

// formatSnapshotRef encodes a (blob-ref, image, cmd) triple into a
// 3-segment SnapshotRef.
//
//	<state-blobs-ref>@<image-ref>@<base64-json-of-cmd-spec>
func formatSnapshotRef(blobRef, image string, cmd snapshotCmdSpec) (container.SnapshotRef, error) {
	raw, err := json.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("container/docker: formatSnapshotRef: marshal cmd: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	return container.SnapshotRef(blobRef + "@" + image + "@" + enc), nil
}

// parseSnapshotRef splits a SnapshotRef into blob-ref, image, cmd-spec.
// First '@' ends blob-ref (no '@' in awf-d1:sha256:hex); last '@' starts
// the base64 cmd-segment (base64 alphabet excludes '@'); the middle is
// the image ref (may itself contain '@' for a registry-digest form).
func parseSnapshotRef(ref container.SnapshotRef) (blobRef, image string, cmd snapshotCmdSpec, err error) {
	s := string(ref)
	first := strings.Index(s, "@")
	last := strings.LastIndex(s, "@")
	if first < 0 || last <= first {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: need 3 '@'-separated segments in %q", s)
	}
	blobRef, image, enc := s[:first], s[first+1:last], s[last+1:]
	if !strings.HasPrefix(blobRef, "awf-d1:sha256:") {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: blob portion %q must have awf-d1:sha256: prefix", blobRef)
	}
	if image == "" {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: empty image portion in %q", s)
	}
	if enc == "" {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: empty cmd portion in %q", s)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: cmd base64: %w", err)
	}
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: cmd json: %w", err)
	}
	return blobRef, image, cmd, nil
}

// cappedWriter wraps an io.Writer with a running-total cap. Returns
// *ErrSnapshotTooLarge if the next Write would push past limit.
type cappedWriter struct {
	w     io.Writer
	n     int64
	limit int64
	path  string
}

func (cw *cappedWriter) Write(p []byte) (int, error) {
	next := cw.n + int64(len(p))
	if next > cw.limit {
		return 0, &ErrSnapshotTooLarge{Path: cw.path, Size: next, Limit: cw.limit}
	}
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

// diffTarWriter is the streaming gzip-tar builder Snapshot drives. Each
// WriteRegular/WriteSymlink/WriteDir call writes one tar entry through
// the gzip → capped-writer pipeline. Intermediate buffers stay at
// ~64 KiB regardless of workspace size. Caller MUST call Close to
// finalize gzip framing.
//
// Note on peak memory: the FINAL compressed blob is held in the underlying
// io.Writer (Snapshot's bytes.Buffer) for state.Blobs.Put. Peak memory at
// Snapshot is bounded by snapshotMaxBlobBytes at Put time, NOT by the
// streaming intermediate buffers.
type diffTarWriter struct {
	cw *cappedWriter
	gw *gzip.Writer
	tw *tar.Writer
}

func newDiffTarWriter(w io.Writer, limit int64) *diffTarWriter {
	cw := &cappedWriter{w: w, limit: limit}
	gw := gzip.NewWriter(cw)
	tw := tar.NewWriter(gw)
	return &diffTarWriter{cw: cw, gw: gw, tw: tw}
}

func (dw *diffTarWriter) WriteRegular(path string, body io.Reader, size int64) error {
	dw.cw.path = path
	hdr := &tar.Header{Name: path, Typeflag: tar.TypeReg, Mode: 0o644, Size: size}
	if err := dw.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(dw.tw, body); err != nil {
		return err
	}
	return nil
}

func (dw *diffTarWriter) WriteSymlink(path, target string) error {
	dw.cw.path = path
	hdr := &tar.Header{Name: path, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}
	return dw.tw.WriteHeader(hdr)
}

func (dw *diffTarWriter) WriteDir(path string, mode int64) error {
	dw.cw.path = path
	hdr := &tar.Header{Name: path, Typeflag: tar.TypeDir, Mode: mode}
	return dw.tw.WriteHeader(hdr)
}

func (dw *diffTarWriter) WriteDeletes(deletes []string) error {
	if len(deletes) == 0 {
		return nil
	}
	dw.cw.path = deletesManifestPath
	manifest := strings.Join(deletes, "\n") + "\n"
	hdr := &tar.Header{Name: deletesManifestPath, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(manifest))}
	if err := dw.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := dw.tw.Write([]byte(manifest))
	return err
}

// Close finalizes the tar AND gzip frames in order. gzip MUST close
// AFTER tar (gzip framing finalization).
func (dw *diffTarWriter) Close() error {
	if err := dw.tw.Close(); err != nil {
		return err
	}
	return dw.gw.Close()
}

// tarReaderForTest is a thin test-side wrapper around archive/tar.
type tarReaderForTest struct {
	tr *tar.Reader
}

func newTarReaderForTest(r io.Reader) *tarReaderForTest {
	return &tarReaderForTest{tr: tar.NewReader(r)}
}

func (t *tarReaderForTest) next() (hdr *tar.Header, body []byte, done bool, err error) {
	hdr, err = t.tr.Next()
	if err == io.EOF {
		return nil, nil, true, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if hdr.Size > 0 {
		body, err = io.ReadAll(t.tr)
		if err != nil {
			return nil, nil, false, err
		}
	}
	return hdr, body, false, nil
}
