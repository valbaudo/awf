package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"

	dockerContainer "github.com/docker/docker/api/types/container"
)

type extractOwnership bool

const (
	preserveArchiveOwnership extractOwnership = false
	ownByContainerUser       extractOwnership = true
)

// extractToContainer is the single Docker archive-extraction seam. Callers
// choose explicitly whether archive ownership is preserved or normalized to
// the configured container user.
func (b *Backend) extractToContainer(ctx context.Context, id, dst string, r io.Reader, ownership extractOwnership) error {
	return b.cli.CopyToContainer(ctx, id, dst, r, dockerContainer.CopyToContainerOptions{
		CopyUIDGID: bool(ownership),
	})
}

// prepareRuntimeDirs provisions AWF's writable runtime directories. The tar
// is rooted at / and ownership is normalized to the configured container user,
// making the operation idempotent for both root and non-root images.
func (b *Backend) prepareRuntimeDirs(ctx context.Context, containerID string) error {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"work/.awf/", "tmp/awf/"} {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}); err != nil {
			return fmt.Errorf("container/docker: prepareRuntimeDirs: tar %q: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("container/docker: prepareRuntimeDirs: close tar: %w", err)
	}
	if err := b.extractToContainer(ctx, containerID, "/", bytes.NewReader(archive.Bytes()), ownByContainerUser); err != nil {
		return fmt.Errorf("container/docker: prepareRuntimeDirs: extract: %w", err)
	}
	return nil
}
