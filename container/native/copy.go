package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valbaudo/awf/container"
)

const (
	inputFileMode os.FileMode = 0o644 // staged file mode (regular, user-readable; cf. docker/copy.go)
	inputDirMode  os.FileMode = 0o755 // mode for created parent directories
)

// CopyTo writes each InputFile to the host filesystem — the symmetric inverse
// of CaptureFiles. Path resolution matches CaptureFiles: absolute paths are
// used literally on the host (no chroot — the no-isolation design, decision
// 10), relative paths resolve via filepath.Join against the handle's workdir.
// Parent directories are created (0o755) so a nested destination "just works",
// mirroring docker's auto-mkdir. Overwrites an existing file. len(files)==0 is
// a no-op; unknown handle is a hard error. Never touches state.Blobs.
//
// `..` traversal: filepath.Join cleans `..` segments, so a destination like
// "../etc/foo" writes to the host literally. Consistent with CaptureFiles and
// Cmd.Run, which are equally unconstrained.
func (b *Backend) CopyTo(ctx context.Context, h container.Handle, files []container.InputFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return errors.New("container/native: CopyTo: unknown handle")
	}
	for _, in := range files {
		resolved := in.Path
		if !filepath.IsAbs(in.Path) {
			resolved = filepath.Join(r.workdir, in.Path)
		}
		if err := os.MkdirAll(filepath.Dir(resolved), inputDirMode); err != nil {
			return fmt.Errorf("container/native: CopyTo: mkdir for %q: %w", in.Path, err)
		}
		if err := os.WriteFile(resolved, in.Content, inputFileMode); err != nil {
			return fmt.Errorf("container/native: CopyTo: %q: %w", in.Path, err)
		}
	}
	return nil
}
