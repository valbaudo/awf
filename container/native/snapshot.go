package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
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

// Restore re-materializes a container workdir from a SnapshotRef. EVERY
// filesystem op goes through one os.Root rooted at the (trusted, fixed) workdir,
// so a '..' name or an attacker-planted/captured symlink cannot redirect a write
// outside the workdir (TOCTOU-safe at openat; the same primitive loader.go uses).
// Perms are set AT CREATE (no post-create Root.Chmod) to avoid CVE-2026-32282.
func (b *Backend) Restore(ctx context.Context, ref container.SnapshotRef, name string) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}
	if name == "" {
		return container.Handle{}, fmt.Errorf("container/native: Restore: name is required")
	}
	if b.blobs == nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: no blob store (construct with native.WithBlobs)")
	}
	if !filepath.IsLocal(name) { // rejects "", "..", "/abs", "a/../../b"; defense-in-depth before OpenRoot
		return container.Handle{}, fmt.Errorf("container/native: Restore: unsafe container name %q", name)
	}
	workdir := filepath.Join(b.workdirRoot, name)

	rootDir, err := os.OpenRoot(b.workdirRoot)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: open root: %w", err)
	}
	defer func() { _ = rootDir.Close() }()

	if err := rootDir.RemoveAll(name); err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: remove %q: %w", name, err)
	}
	if err := rootDir.MkdirAll(name, 0o755); err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: mkdir %q: %w", name, err)
	}

	if err := b.extractInto(rootDir, name, ref); err != nil {
		_ = os.RemoveAll(workdir) // single cleanup path
		return container.Handle{}, err
	}

	b.mu.Lock()
	b.handles[workdir] = nativeHandle{workdir: workdir}
	b.mu.Unlock()
	return container.Handle{Name: name, ID: workdir}, nil
}

// extractInto streams the blob's gzip-tar into <root>/<base> via the root.
// Task 6 wraps the decompressor in the three decompression caps.
func (b *Backend) extractInto(root *os.Root, base string, ref container.SnapshotRef) error {
	blob, err := b.blobs.Get(string(ref))
	if err != nil {
		return fmt.Errorf("container/native: Restore: blobs.Get: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("container/native: Restore: gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("container/native: Restore: tar: %w", err)
		}
		rel := path.Join(base, path.Clean("/"+hdr.Name)) // join under base; leading-slash-stripped
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, fs.FileMode(hdr.Mode)&os.ModePerm); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("container/native: Restore: mkdir %q: %w", rel, err)
			}
		case tar.TypeReg:
			if err := ensureParent(root, rel); err != nil {
				return err
			}
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return fmt.Errorf("container/native: Restore: create %q: %w", rel, err)
			}
			_, cpErr := io.Copy(f, tr) // Task 6: replace with capped reader + io.CopyN
			_ = f.Close()
			if cpErr != nil {
				return fmt.Errorf("container/native: Restore: write %q: %w", rel, cpErr)
			}
		case tar.TypeSymlink:
			if err := ensureParent(root, rel); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, rel); err != nil { // target verbatim (decision: store-verbatim)
				return fmt.Errorf("container/native: Restore: symlink %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("container/native: Restore: unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// ensureParent creates rel's parent directories through the root (confined).
func ensureParent(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("container/native: Restore: mkdir parent %q: %w", dir, err)
	}
	return nil
}
