package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"strings"

	dockerContainer "github.com/docker/docker/api/types/container"

	"github.com/valbaudo/awf/container"
)

// inputFileMode is the tar entry mode for staged input_files: a regular,
// user-readable file. The step's Cmd controls executability, not the channel.
const inputFileMode int64 = 0o644

// CopyTo writes each InputFile via one CopyToContainer call rooted at "/". The
// tar is built in a goroutine and piped to the SDK in lockstep (the io.Pipe +
// CloseWithError pattern from Restore, snapshot.go). Entry names are the dest
// paths with the leading "/" stripped. moby auto-creates ancestor directories
// for each entry (go-archive createImpliedDirectories), so no TypeDir entries
// are emitted; "/" (the dstPath) always exists. Overwrites. len==0 is a no-op.
func (b *Backend) CopyTo(ctx context.Context, h container.Handle, files []container.InputFile) error {
	r, err := b.lookupRegistered(ctx, "CopyTo", h)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	dockerID, err := b.resolveContainerID(ctx, h, r)
	if err != nil {
		return fmt.Errorf("container/docker: CopyTo: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		var werr error
		for _, in := range files {
			hdr := &tar.Header{
				Name:     strings.TrimPrefix(in.Path, "/"),
				Mode:     inputFileMode,
				Size:     int64(len(in.Content)),
				Typeflag: tar.TypeReg,
			}
			if werr = tw.WriteHeader(hdr); werr != nil {
				break
			}
			if _, werr = tw.Write(in.Content); werr != nil {
				break
			}
		}
		if werr == nil {
			werr = tw.Close()
		}
		_ = pw.CloseWithError(werr)
	}()

	copyErr := b.cli.CopyToContainer(ctx, dockerID, "/", pr, dockerContainer.CopyToContainerOptions{})
	_ = pr.CloseWithError(copyErr)
	if copyErr != nil {
		return fmt.Errorf("container/docker: CopyTo: CopyToContainer: %w", copyErr)
	}
	return nil
}
