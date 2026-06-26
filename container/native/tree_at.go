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
	"math"
	"os"
	"path"

	"github.com/valbaudo/awf/container"
)

// ReadTreeAt captures the subtree rooted at dir (workdir-relative) as a
// deterministic gzip-tar. Entry paths in the archive are relative to dir —
// matching the convention used by the Fake backend and BuildTreeTar.
//
// Confinement: os.OpenRoot(r.workdir) is used for all filesystem operations.
// dir is normalised via path.Join(".", path.Clean("/"+dir)) so ".." components
// collapse before the root is opened; os.Root then refuses any symlink that
// resolves outside the workdir at read/stat time. Both layers together make
// escapes impossible regardless of the workdir contents.
//
// Returns an error when dir does not exist inside the handle's workdir.
func (b *Backend) ReadTreeAt(ctx context.Context, h container.Handle, dir string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("container/native: ReadTreeAt: unknown handle %q", h.ID)
	}

	root, err := os.OpenRoot(r.workdir)
	if err != nil {
		return nil, fmt.Errorf("container/native: ReadTreeAt: open root: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Normalise: strip any leading slash so the result is workdir-relative and
	// ".." components cannot escape (mirrors the file_at.go idiom).
	rel := path.Join(".", path.Clean("/"+dir))

	// Verify dir exists before walking. Missing dir is a hard error.
	if _, err := root.Stat(rel); err != nil {
		return nil, fmt.Errorf("container/native: ReadTreeAt: %q: %w", dir, err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf) // bare header: no Name, no mtime, OS byte 255 → deterministic
	tw := tar.NewWriter(gw)

	walkErr := fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == rel {
			return nil // skip the subtree-root directory entry itself
		}

		// Strip the leading rel prefix to produce a path relative to dir.
		// All descendants have paths of the form rel+"/"+ something.
		var relToDir string
		if rel == "." {
			relToDir = p
		} else {
			relToDir = p[len(rel)+1:] // safe: p is always a descendant of rel
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.IsDir():
			return tw.WriteHeader(tarHeader(relToDir+"/", tar.TypeDir, info.Mode(), 0, ""))
		case d.Type()&fs.ModeSymlink != 0:
			// WalkDir does NOT follow symlinks (d.IsDir() is false for symlinks
			// even if the target is a directory). Read the link target verbatim.
			target, err := root.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(tarHeader(relToDir, tar.TypeSymlink, info.Mode(), 0, target))
		case info.Mode().IsRegular():
			if err := tw.WriteHeader(tarHeader(relToDir, tar.TypeReg, info.Mode(), info.Size(), "")); err != nil {
				return err
			}
			f, err := root.Open(p)
			if err != nil {
				return err
			}
			_, cpErr := io.Copy(tw, f)
			_ = f.Close()
			return cpErr
		default:
			return nil // fifo/socket/device — skip
		}
	})
	if walkErr != nil {
		return nil, fmt.Errorf("container/native: ReadTreeAt: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteTreeAt extracts a gzip-tar (as produced by ReadTreeAt or BuildTreeTar)
// into dir (workdir-relative), creating dir and its parents if needed.
// Existing files are overwritten. Entry paths in the archive are relative to dir.
// Honors b.snapshotMaxRestoreBytes and b.maxEntries as zip-bomb guards.
//
// Confinement is identical to ReadTreeAt — os.OpenRoot(r.workdir) plus the
// same dir normalisation. The handle is validated BEFORE any tar decoding.
// A tar entry whose follow-through traverses an escaping symlink is refused
// by os.Root at MkdirAll/OpenFile time.
func (b *Backend) WriteTreeAt(ctx context.Context, h container.Handle, dir string, tarGz []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Validate handle BEFORE tar work — mirror file_at.go's lock-lookup-proceed order.
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("container/native: WriteTreeAt: unknown handle %q", h.ID)
	}

	root, err := os.OpenRoot(r.workdir)
	if err != nil {
		return fmt.Errorf("container/native: WriteTreeAt: open root: %w", err)
	}
	defer func() { _ = root.Close() }()

	cleanedDir := path.Join(".", path.Clean("/"+dir))

	// Ensure the target directory (and its parents) exist.
	if err := root.MkdirAll(cleanedDir, 0o755); err != nil {
		return fmt.Errorf("container/native: WriteTreeAt: mkdir %q: %w", dir, err)
	}

	// Decompress then extract under cleanedDir with the same three caps as
	// extractInto: a cappedReader (cumulative decompressed bytes), a per-file
	// CopyN bound, and an entry-count guard.
	gr, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return fmt.Errorf("container/native: WriteTreeAt: gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	capped := &cappedReader{r: gr, limit: b.snapshotMaxRestoreBytes}
	tr := tar.NewReader(capped)
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("container/native: WriteTreeAt: tar: %w", err)
		}
		entries++
		if entries > b.maxEntries {
			return &nativeSnapshotTooLarge{n: int64(entries), limit: int64(b.maxEntries)}
		}

		// Join cleanedDir with the entry's cleaned path (leading-slash-stripped),
		// confining ".." and absolute paths to the subtree.
		rel := path.Join(cleanedDir, path.Clean("/"+hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, fs.FileMode(hdr.Mode)&os.ModePerm); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("container/native: WriteTreeAt: mkdir %q: %w", rel, err)
			}
		case tar.TypeReg:
			if err := writeTreeEnsureParent(root, rel); err != nil {
				return err
			}
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return fmt.Errorf("container/native: WriteTreeAt: create %q: %w", rel, err)
			}
			// Per-file cap: mirrors extractInto's defense-in-depth; cappedReader is the real budget.
			perFile := b.snapshotMaxRestoreBytes
			if perFile < math.MaxInt64 {
				perFile++ // +1 distinguishes at-cap from over-cap; clamp avoids int64 overflow
			}
			_, cpErr := io.CopyN(f, tr, perFile)
			closeErr := f.Close()
			if cpErr != nil && cpErr != io.EOF {
				return fmt.Errorf("container/native: WriteTreeAt: write %q: %w", rel, cpErr)
			}
			if closeErr != nil {
				return fmt.Errorf("container/native: WriteTreeAt: close %q: %w", rel, closeErr)
			}
		case tar.TypeSymlink:
			if err := writeTreeEnsureParent(root, rel); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, rel); err != nil { // target verbatim (store-verbatim decision)
				return fmt.Errorf("container/native: WriteTreeAt: symlink %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("container/native: WriteTreeAt: unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// writeTreeEnsureParent creates rel's parent directories through root (confined).
// Inlined (not reusing ensureParent from snapshot.go) so the error prefix is
// accurate for WriteTreeAt callers.
func writeTreeEnsureParent(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("container/native: WriteTreeAt: mkdir parent %q: %w", dir, err)
	}
	return nil
}
