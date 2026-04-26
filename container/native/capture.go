package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valbaudo/awf/container"
)

// CaptureFiles reads the named in-container paths and returns their
// content. Relative paths resolve via filepath.Join against the
// handle's workdir; absolute paths are used literally on the host
// filesystem (no chroot — consistent with the no-isolation design,
// decision 10). Missing-path is a hard error (no partial returns)
// matching docker's CaptureFiles behavior.
//
// CaptureFiles `..` traversal: filepath.Join cleans `..` segments,
// so `CaptureFiles(h, []string{"../etc/passwd"})` resolves to
// `<host>/etc/passwd`. This is expected behavior — Cmd.Run is equally
// unconstrained.
func (b *Backend) CaptureFiles(ctx context.Context, h container.Handle, paths []string) ([]container.CapturedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return nil, errors.New("container/native: CaptureFiles: unknown handle")
	}
	out := make([]container.CapturedFile, 0, len(paths))
	for _, p := range paths {
		resolved := p
		if !filepath.IsAbs(p) {
			resolved = filepath.Join(r.workdir, p)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("container/native: CaptureFiles: %q: %w", p, err)
		}
		out = append(out, container.CapturedFile{Path: p, Content: data})
	}
	return out, nil
}
