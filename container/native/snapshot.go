package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/valbaudo/awf/container"
)

// tarHeader builds a DETERMINISTIC tar header: zero mtime/atime/ctime and zero
// owner identity (uid/gid/uname/gname), so identical workspace content yields a
// byte-identical archive (the blob = content-hash invariant). Never use
// tar.FileInfoHeader — it leaks the runner's uid/gid/uname/gname + real mtime.
// For reg/dir the mode is masked to permission bits (exec preserved;
// setuid/setgid/sticky stripped). Format is left unset so archive/tar picks the
// minimal per-entry format (USTAR for short paths, PAX only when a path is long
// or a file is large) — deterministic given zeroed time/owner, and unlike a
// forced USTAR it does not fail on long paths or files > 8 GiB.
func tarHeader(name string, typeflag byte, mode fs.FileMode, size int64, linkname string) *tar.Header {
	h := &tar.Header{Name: name, Typeflag: typeflag, Linkname: linkname}
	switch typeflag {
	case tar.TypeReg:
		h.Mode = int64(mode & os.ModePerm)
		h.Size = size
	case tar.TypeDir:
		h.Mode = int64(mode & os.ModePerm)
	case tar.TypeSymlink:
		h.Mode = 0o777 // conventional; symlink perms are ignored by the kernel
	}
	return h
}

// nativeSnapshotTooLarge reports container.ErrSnapshotTooLarge via errors.Is so
// the engine classifies a too-large snapshot as permanent_failure without
// importing this concrete type.
type nativeSnapshotTooLarge struct{ n, limit int64 }

func (e *nativeSnapshotTooLarge) Error() string {
	return fmt.Sprintf("container/native: snapshot exceeds size limit: %d > %d bytes", e.n, e.limit)
}
func (e *nativeSnapshotTooLarge) Is(target error) bool {
	return target == container.ErrSnapshotTooLarge
}

// cappedWriter trips *nativeSnapshotTooLarge once cumulative bytes exceed limit.
type cappedWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, &nativeSnapshotTooLarge{n: c.n + int64(len(p)), limit: c.limit}
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Snapshot captures the container workdir as a deterministic gzip-tar blob.
// Enforces TWO caps: the compressed-output cap (cappedWriter on the gzip stream)
// and the decompressed-total cap (uncompressed bytes fed in) — the latter
// symmetric with Restore so an unrestorable snapshot cannot be created.
func (b *Backend) Snapshot(ctx context.Context, h container.Handle) (container.SnapshotRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("container/native: Snapshot: unknown handle %q", h.ID)
	}
	if b.blobs == nil {
		return "", fmt.Errorf("container/native: Snapshot: no blob store (construct with native.WithBlobs)")
	}

	var buf bytes.Buffer
	cw := &cappedWriter{w: &buf, limit: b.snapshotMaxBlobBytes}
	gw := gzip.NewWriter(cw) // bare header: no Name, no mtime, OS byte 255
	tw := tar.NewWriter(gw)

	var uncompressed int64
	entries := 0
	walkErr := filepath.WalkDir(r.workdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == r.workdir { // skip the root entry
			return nil
		}
		rel, err := filepath.Rel(r.workdir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entries++
		if entries > snapshotMaxEntries {
			return &nativeSnapshotTooLarge{n: int64(entries), limit: int64(snapshotMaxEntries)}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(tarHeader(rel+"/", tar.TypeDir, info.Mode(), 0, ""))
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p) // WalkDir does NOT follow symlinks; captured verbatim
			if err != nil {
				return err
			}
			return tw.WriteHeader(tarHeader(rel, tar.TypeSymlink, info.Mode(), 0, target))
		case info.Mode().IsRegular():
			uncompressed += info.Size()
			if uncompressed > b.snapshotMaxRestoreBytes {
				return &nativeSnapshotTooLarge{n: uncompressed, limit: b.snapshotMaxRestoreBytes}
			}
			if err := tw.WriteHeader(tarHeader(rel, tar.TypeReg, info.Mode(), info.Size(), "")); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, cpErr := io.Copy(tw, f)
			_ = f.Close()
			return cpErr
		default:
			return nil // fifo/socket/device skipped
		}
	})
	if walkErr != nil {
		return "", fmt.Errorf("container/native: Snapshot: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	ref, err := b.blobs.Put(buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("container/native: Snapshot: blobs.Put: %w", err)
	}
	return container.SnapshotRef(ref), nil
}
