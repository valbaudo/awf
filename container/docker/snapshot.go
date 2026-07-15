package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	dockerContainer "github.com/docker/docker/api/types/container"

	"github.com/valbaudo/awf/container"
)

// deletesManifestPath is the fixed in-tar path of the deletes sidecar file.
const deletesManifestPath = ".awf-deletes"

// snapshotBlobScheme must equal ir.DigestScheme; pinned by a test.
const snapshotBlobScheme = "awf-d1:sha256:"

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

// Is reports the docker too-large error as the package-level
// container.ErrSnapshotTooLarge sentinel, so the engine can classify it as a
// permanent_failure via errors.Is without importing this concrete type
// (Phase-4 decision 11; keeps the classification behind the container seam).
func (e *ErrSnapshotTooLarge) Is(target error) bool {
	return target == container.ErrSnapshotTooLarge
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
	if !strings.HasPrefix(blobRef, snapshotBlobScheme) {
		return "", "", snapshotCmdSpec{}, fmt.Errorf("container/docker: parseSnapshotRef: blob portion %q must have %q prefix", blobRef, snapshotBlobScheme)
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

// Snapshot captures the workspace state of an image-mode container as a
// gzip-compressed CoW diff (Phase 4 design decision 4 + slice 4.4 Design
// Q5/Q8):
//
//  1. ContainerInspect — read effective Config.Image, Config.Cmd,
//     Config.Entrypoint.
//  2. ContainerDiff — list changed paths.
//  3. Sort changes by Path (deterministic blob refs).
//  4. For each ChangeAdd/ChangeModify:
//     CopyFromContainer → PathStat dispatch (dir/symlink/regular)
//     → stream body through dw.WriteRegular/WriteSymlink/WriteDir.
//  5. For each ChangeDelete: accumulate into deletes slice.
//  6. dw.WriteDeletes(deletes); dw.Close.
//  7. b.blobs.Put(blob bytes) → blobRef.
//  8. formatSnapshotRef(blobRef, image, cmdSpec) → SnapshotRef.
//
// Compose-mode handles error explicitly.
func (b *Backend) Snapshot(ctx context.Context, h container.Handle) (container.SnapshotRef, error) {
	r, err := b.lookupRegistered(ctx, "Snapshot", h)
	if err != nil {
		return "", err
	}
	if r.kind != kindImage {
		return "", fmt.Errorf("container/docker: Snapshot: handle kind %q not supported (image-mode only; compose snapshot is out-of-scope per Phase 4 design)", r.kind)
	}

	info, err := b.cli.ContainerInspect(ctx, r.dockerID)
	if err != nil {
		return "", fmt.Errorf("container/docker: Snapshot: ContainerInspect: %w", err)
	}
	if info.Config == nil {
		return "", fmt.Errorf("container/docker: Snapshot: ContainerInspect returned nil Config (daemon bug)")
	}
	image := info.Config.Image
	if image == "" {
		return "", fmt.Errorf("container/docker: Snapshot: ContainerInspect returned empty Config.Image")
	}
	cmdSpec := snapshotCmdSpec{
		Cmd:        []string(info.Config.Cmd),
		Entrypoint: []string(info.Config.Entrypoint),
	}

	changes, err := b.cli.ContainerDiff(ctx, r.dockerID)
	if err != nil {
		return "", fmt.Errorf("container/docker: Snapshot: ContainerDiff: %w", err)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	var blobBuf bytes.Buffer
	dw := newDiffTarWriter(&blobBuf, b.snapshotMaxBlobBytes)

	var deletes []string
	for _, ch := range changes {
		switch ch.Kind {
		case dockerContainer.ChangeAdd, dockerContainer.ChangeModify:
			if err := b.captureOneIntoDiffTar(ctx, r.dockerID, ch.Path, dw); err != nil {
				return "", err
			}
		case dockerContainer.ChangeDelete:
			deletes = append(deletes, ch.Path)
		}
	}
	if err := dw.WriteDeletes(deletes); err != nil {
		return "", err
	}
	if err := dw.Close(); err != nil {
		return "", err
	}

	blobRef, err := b.blobs.Put(blobBuf.Bytes())
	if err != nil {
		return "", fmt.Errorf("container/docker: Snapshot: blobs.Put: %w", err)
	}
	return formatSnapshotRef(blobRef, image, cmdSpec)
}

// captureOneIntoDiffTar streams one container path into the diff-tar via
// CopyFromContainer + PathStat-based dispatch. Block/char/fifo paths
// (unusual in workspaces) are skipped with no error.
func (b *Backend) captureOneIntoDiffTar(ctx context.Context, containerID, path string, dw *diffTarWriter) error {
	rc, stat, err := b.cli.CopyFromContainer(ctx, containerID, path)
	if err != nil {
		return fmt.Errorf("container/docker: Snapshot: CopyFromContainer %q: %w", path, err)
	}
	defer func() { _ = rc.Close() }()

	if stat.Mode.IsDir() {
		return dw.WriteDir(path, int64(stat.Mode.Perm()))
	}
	if stat.Mode&os.ModeSymlink != 0 {
		return dw.WriteSymlink(path, stat.LinkTarget)
	}
	if !stat.Mode.IsRegular() {
		return nil
	}

	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		return fmt.Errorf("container/docker: Snapshot: read tar header for %q: %w", path, err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("container/docker: Snapshot: %q: tar header Typeflag = %d, want TypeReg", path, hdr.Typeflag)
	}
	return dw.WriteRegular(path, tr, hdr.Size)
}

// Restore re-materializes a container from a SnapshotRef. Streams the
// gzip-compressed diff through an io.Pipe writer goroutine into
// CopyToContainer (no intermediate plain-tar buffer; peak memory at this
// stage is the diff bytes already in RAM from Blobs.Get + ~74 KiB
// streaming buffers).
//
// The embedded image is NOT auto-pulled; callers responsible for prior
// ImagePull (matches Backend.Create's image-mode behavior — slice 4.1
// precedent). If the image is absent from the local cache, ContainerCreate
// errors with "no such image" and Restore propagates.
//
// Per-delete Exec is O(N) sequential. A workspace with N deletes does N
// sequential rm -rf calls (~50-100ms each); a 1000-delete workspace takes
// ~50-100 seconds. Acceptable for slice 4.4 (Restore is rare); a future
// optimization could batch via xargs. If a delete-Exec fails, Restore
// aborts cleanly (force-removes the partially-restored container) — the
// engine resume re-calls Restore from scratch.
func (b *Backend) Restore(ctx context.Context, ref container.SnapshotRef, name string) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}
	if name == "" {
		return container.Handle{}, fmt.Errorf("container/docker: Restore: name is required (IR container name)")
	}

	blobRef, image, cmdSpec, err := parseSnapshotRef(ref)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/docker: Restore: %w", err)
	}

	diffBytes, err := b.blobs.Get(blobRef)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/docker: Restore: blobs.Get(%s): %w", blobRef, err)
	}

	containerNameStr := containerName(b.runID, name)
	cfg := &dockerContainer.Config{
		Image:      image,
		Cmd:        cmdSpec.Cmd,
		Entrypoint: cmdSpec.Entrypoint,
	}
	resp, err := b.cli.ContainerCreate(ctx, cfg, &dockerContainer.HostConfig{}, nil, nil, containerNameStr)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/docker: Restore: ContainerCreate: %w", err)
	}

	// Stream the diff via io.Pipe: one goroutine writes plain tar to pw;
	// CopyToContainer reads from pr in lockstep.
	deletesCh := make(chan []string, 1)
	pr, pw := io.Pipe()
	go func() {
		var deletes []string
		// deletesCh send happens in defer so a panic in streamPlainTarFromDiff
		// doesn't deadlock the main goroutine waiting on <-deletesCh.
		defer func() {
			_ = pw.Close()
			deletesCh <- deletes
		}()
		out, err := streamPlainTarFromDiff(pw, diffBytes)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		deletes = out
	}()

	// Snapshot headers intentionally do not retain archive UID/GID fidelity.
	// Normalize restored workspace content to the image's configured user,
	// matching CopyTo/WriteFileAt/WriteTreeAt and the single-user workspace
	// contract.
	copyErr := b.extractToContainer(ctx, resp.ID, "/", pr, ownByContainerUser)
	// Close the reader so the writer goroutine unblocks even if CopyToContainer
	// aborted mid-stream (ctx-cancel, daemon error). Without this, pw.Write
	// inside streamPlainTarFromDiff would block forever and we'd deadlock on
	// the <-deletesCh receive below.
	_ = pr.CloseWithError(copyErr)
	deletes := <-deletesCh
	if copyErr != nil {
		return b.restoreFail(ctx, resp.ID, "CopyToContainer", copyErr)
	}
	if err := b.prepareRuntimeDirs(ctx, resp.ID); err != nil {
		return b.restoreFail(ctx, resp.ID, "prepareRuntimeDirs", err)
	}

	// Extraction happens before startup so entrypoints observe the restored
	// workspace atomically and cannot race the snapshot copy.
	if err := b.cli.ContainerStart(ctx, resp.ID, dockerContainer.StartOptions{}); err != nil {
		return b.restoreFail(ctx, resp.ID, "ContainerStart", err)
	}
	if err := b.waitReady(ctx, resp.ID); err != nil {
		return b.restoreFail(ctx, resp.ID, "waitReady", err)
	}

	for _, del := range deletes {
		chunks, resultCh, execErr := b.execImage(ctx, resp.ID, container.Cmd{Run: "rm -rf -- " + shellQuotePath(del)})
		if execErr != nil {
			return b.restoreFail(ctx, resp.ID, fmt.Sprintf("delete %q", del), execErr)
		}
		// Drain chunks (slice 5.3 streaming contract); the rm command's
		// output is irrelevant to Restore, but the channel must be drained
		// before the result is delivered.
		for range chunks {
		}
		result := <-resultCh
		if result.Err != nil {
			return b.restoreFail(ctx, resp.ID, fmt.Sprintf("delete %q", del), result.Err)
		}
		if result.ExitCode != 0 {
			return b.restoreFail(ctx, resp.ID, fmt.Sprintf("delete %q exited %d", del, result.ExitCode), nil)
		}
	}

	b.mu.Lock()
	b.handles[resp.ID] = registeredContainer{kind: kindImage, dockerID: resp.ID}
	b.mu.Unlock()

	return container.Handle{Name: name, ID: resp.ID}, nil
}

// restoreFail force-removes the partially-created container and returns a
// wrapped error for the caller. Used by Restore's mid-flight failure paths
// (ContainerStart, waitReady, CopyToContainer, per-delete Exec). When cause
// is nil, the message itself carries the diagnostic.
func (b *Backend) restoreFail(ctx context.Context, containerID, stage string, cause error) (container.Handle, error) {
	_ = b.cli.ContainerRemove(ctx, containerID, dockerContainer.RemoveOptions{Force: true})
	if cause != nil {
		return container.Handle{}, fmt.Errorf("container/docker: Restore: %s: %w", stage, cause)
	}
	return container.Handle{}, fmt.Errorf("container/docker: Restore: %s", stage)
}

// Decision: docker Restore is intentionally UNbounded (no decompression-bomb
// caps like container/native's). CopyToContainer extracts into the isolated
// container fs, so a bomb is a container-disk DoS only, never a host-disk one.
//
// streamPlainTarFromDiff reads a gzipped diff-tar (in RAM) and writes a
// plain-tar (no .awf-deletes sidecar, leading-slash stripped) to w. Returns
// the parsed deletes list separately. Streaming: peak memory bounded to
// the gzip+tar internal buffers (~64 KiB) + the diff-tar bytes already in
// RAM (the input).
func streamPlainTarFromDiff(w io.Writer, diffBytes []byte) ([]string, error) {
	gr, err := gzip.NewReader(bytes.NewReader(diffBytes))
	if err != nil {
		return nil, fmt.Errorf("gzip.NewReader: %w", err)
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	var deletes []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar.Next: %w", err)
		}
		if hdr.Name == deletesManifestPath {
			body, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read deletes: %w", err)
			}
			for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
				if line != "" {
					deletes = append(deletes, line)
				}
			}
			continue
		}
		outHdr := &tar.Header{
			Name:     strings.TrimPrefix(hdr.Name, "/"),
			Typeflag: hdr.Typeflag,
			Mode:     hdr.Mode,
			Linkname: hdr.Linkname,
			Size:     hdr.Size,
		}
		if err := tw.WriteHeader(outHdr); err != nil {
			return nil, fmt.Errorf("tw.WriteHeader: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, fmt.Errorf("tw.Copy: %w", err)
			}
		}
	}
	return deletes, nil
}

// shellQuotePath wraps p in single quotes and escapes embedded single
// quotes. Paired with the `--` argument terminator (`rm -rf -- '<path>'`)
// so paths starting with `-` aren't parsed as flags.
func shellQuotePath(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
