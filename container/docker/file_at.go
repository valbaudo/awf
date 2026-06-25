package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	dockerContainer "github.com/docker/docker/api/types/container"

	"github.com/valbaudo/awf/container"
)

// ReadFileAt reads a single regular file from the running container at the
// given absolute path. Uses CopyFromContainer (which returns a tar stream) and
// extracts the single regular-file entry.
//
// Missing path: the daemon surfaces this as an error from CopyFromContainer;
// we propagate it as a hard error. Container-fs confinement is enforced by the
// Docker daemon boundary (unlike native's os.Root).
func (b *Backend) ReadFileAt(ctx context.Context, h container.Handle, filePath string) ([]byte, error) {
	r, err := b.lookupRegistered(ctx, "ReadFileAt", h)
	if err != nil {
		return nil, err
	}
	dockerID, err := b.resolveContainerID(ctx, h, r)
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadFileAt: %w", err)
	}

	cleanPath := path.Clean(filePath)

	rc, _, err := b.cli.CopyFromContainer(ctx, dockerID, cleanPath)
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadFileAt: CopyFromContainer %q: %w", cleanPath, err)
	}
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("container/docker: ReadFileAt: read tar %q: %w", cleanPath, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("container/docker: ReadFileAt: read tar body %q: %w", cleanPath, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("container/docker: ReadFileAt: no regular-file entry found in tar for %q", cleanPath)
}

// WriteFileAt writes content into the running container at the given absolute
// path, overwriting any existing file. Mirrors CopyTo in copy.go: builds a
// one-entry tar via an io.Pipe goroutine and calls CopyToContainer with the
// path rooted at "/". The Docker daemon's moby layer auto-creates any missing
// ancestor directories.
//
// Container-fs confinement is enforced by the Docker daemon boundary (unlike
// native's os.Root).
func (b *Backend) WriteFileAt(ctx context.Context, h container.Handle, filePath string, content []byte) error {
	r, err := b.lookupRegistered(ctx, "WriteFileAt", h)
	if err != nil {
		return err
	}
	dockerID, err := b.resolveContainerID(ctx, h, r)
	if err != nil {
		return fmt.Errorf("container/docker: WriteFileAt: %w", err)
	}

	cleanPath := path.Clean(filePath)
	// CopyToContainer is rooted at "/"; strip the leading slash so the tar
	// entry name is relative (moby's CopyToContainer prepends the dstPath).
	entryName := strings.TrimPrefix(cleanPath, "/")

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		hdr := &tar.Header{
			Name:     entryName,
			Mode:     inputFileMode,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		var werr error
		if werr = tw.WriteHeader(hdr); werr == nil {
			_, werr = io.Copy(tw, bytes.NewReader(content))
		}
		if werr == nil {
			werr = tw.Close()
		}
		_ = pw.CloseWithError(werr)
	}()

	copyErr := b.cli.CopyToContainer(ctx, dockerID, "/", pr, dockerContainer.CopyToContainerOptions{})
	_ = pr.CloseWithError(copyErr)
	if copyErr != nil {
		return fmt.Errorf("container/docker: WriteFileAt: CopyToContainer %q: %w", cleanPath, copyErr)
	}
	return nil
}
