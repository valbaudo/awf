package native

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/valbaudo/awf/container"
)

// ReadFileAt reads the file at path inside the handle's workdir. Confinement
// goes through os.OpenRoot rooted at the workdir — TOCTOU-safe, matching the
// extractInto pattern in snapshot.go. The cleaned path must resolve to a file;
// a missing path is a hard error.
func (b *Backend) ReadFileAt(ctx context.Context, h container.Handle, filePath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("container/native: ReadFileAt: unknown handle %q", h.ID)
	}

	root, err := os.OpenRoot(r.workdir)
	if err != nil {
		return nil, fmt.Errorf("container/native: ReadFileAt: open root: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Confine via the same path-cleaning idiom as extractInto: strip any leading
	// slash then join under an empty base so the result is relative (no "../"
	// escape possible once the leading slash is normalised away).
	rel := path.Join(".", path.Clean("/"+filePath))

	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("container/native: ReadFileAt: %q: %w", filePath, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("container/native: ReadFileAt: read %q: %w", filePath, err)
	}
	return data, nil
}

// WriteFileAt writes content to path inside the handle's workdir, creating
// parent directories as needed. Confinement goes through os.OpenRoot rooted at
// the workdir — TOCTOU-safe, matching the extractInto pattern in snapshot.go.
// Overwrites any existing file.
func (b *Backend) WriteFileAt(ctx context.Context, h container.Handle, filePath string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("container/native: WriteFileAt: unknown handle %q", h.ID)
	}

	root, err := os.OpenRoot(r.workdir)
	if err != nil {
		return fmt.Errorf("container/native: WriteFileAt: open root: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Same confinement idiom as ReadFileAt / extractInto.
	rel := path.Join(".", path.Clean("/"+filePath))

	// Create parent directories under the root (confined).
	if dir := path.Dir(rel); dir != "." {
		if mkErr := root.MkdirAll(dir, fs.FileMode(0o755)); mkErr != nil {
			return fmt.Errorf("container/native: WriteFileAt: mkdir parent %q: %w", dir, mkErr)
		}
	}

	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(0o644))
	if err != nil {
		return fmt.Errorf("container/native: WriteFileAt: create %q: %w", filePath, err)
	}
	_, writeErr := f.Write(content)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("container/native: WriteFileAt: write %q: %w", filePath, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("container/native: WriteFileAt: close %q: %w", filePath, closeErr)
	}
	return nil
}
