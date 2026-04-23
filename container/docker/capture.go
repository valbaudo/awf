package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/valbaudo/awf/container"
)

// CaptureFiles reads each in-container path and returns one CapturedFile per
// path in the input order. Missing-path is a hard error (no partial returns —
// matches the fake / Phase 2 invariant).
//
// Implementation: one CopyFromContainer call per path. The SDK returns a tar
// archive containing the file at path's basename; we iterate the entries,
// find the basename, read its bytes. (For directory sources, the archive
// contains the whole tree — slice 4.2 only captures regular files since
// output_files are spec'd as files, not directories; a future Backend
// extension could add directory capture.)
//
// nil/empty paths is a no-op returning ([], nil) — matches the fake's
// "len-zero loop body" semantic.
func (b *Backend) CaptureFiles(ctx context.Context, h container.Handle, paths []string) ([]container.CapturedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	dockerID, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("container/docker: CaptureFiles: unknown handle %q (not Created or already Destroyed)", h.ID)
	}
	if len(paths) == 0 {
		return nil, nil
	}

	out := make([]container.CapturedFile, 0, len(paths))
	for _, p := range paths {
		content, err := b.captureOne(ctx, dockerID, p)
		if err != nil {
			return nil, fmt.Errorf("container/docker: CaptureFiles: %q: %w", p, err)
		}
		out = append(out, container.CapturedFile{Path: p, Content: content})
	}
	return out, nil
}

// captureOne reads one file from the container and returns its bytes. The
// SDK's CopyFromContainer returns a tar stream; we walk the entries until
// we find a regular file whose basename matches path.Base(p).
// Missing-path surfaces as an error wrapping the requested basename.
//
// Symlinks: if the requested path is a symlink (e.g., an author declares
// `output_files: ["/var/log/app.log"]` and that's a symlink to a different
// file), Docker's daemon (moby v28.5.2 daemon/archive_unix.go) may emit the
// tar entry with TypeSymlink rather than TypeReg. We surface that as a
// "not a regular file" error rather than dereferencing — authors should
// resolve the symlink in their script and declare the resolved path. This
// is consistent with Phase 2's CapturedFile contract (path → bytes, no
// indirection).
func (b *Backend) captureOne(ctx context.Context, containerID, p string) (_ []byte, retErr error) {
	rc, _, err := b.cli.CopyFromContainer(ctx, containerID, p)
	if err != nil {
		return nil, fmt.Errorf("CopyFromContainer: %w", err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close tar stream: %w", cerr)
		}
	}()

	wantName := path.Base(p)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}
		// The tar entry's Name is the basename for a single-file source;
		// for a directory source it's the path relative to the source dir.
		// Slice 4.2 captures only single-file sources; match by basename.
		if path.Base(hdr.Name) != wantName {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("entry %q is not a regular file (typeflag=%d)", hdr.Name, hdr.Typeflag)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read entry body: %w", err)
		}
		return content, nil
	}
	return nil, fmt.Errorf("file not found in archive (looked for basename %q)", wantName)
}
