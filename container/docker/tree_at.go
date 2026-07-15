package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/valbaudo/awf/container"
)

// treeAtCappedReader caps cumulative decompressed bytes during WriteTreeAt to
// guard against gzip-bomb payloads. Mirrors treeTarCappedReader in
// container/treetar.go; defined locally because treeTarCappedReader is
// unexported.
type treeAtCappedReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *treeAtCappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, fmt.Errorf("container/docker: WriteTreeAt: decompressed bytes %d exceed cap %d (possible zip-bomb)", c.n, c.limit)
	}
	return n, err
}

// rerootTarEntries rewrites entry names in a plain (uncompressed) tar:
//
//  1. If stripPrefix is non-empty, strips it from each name. Entries that
//     lack the prefix are skipped silently — this handles the root-sentinel
//     entry Docker includes when CopyFromContainer copies a directory
//     (e.g. "projects/" strips to "" and is dropped). An empty name after
//     stripping is also skipped.
//
//  2. path.Clean is applied and the result is validated: an absolute path,
//     "..", or a "../"-prefixed component is rejected as a path-escape.
//
//  3. addPrefix is prepended to the clean name. Directory entries (TypeDir)
//     always receive a trailing "/".
//
// Entry bodies are copied unchanged. Entry count is capped at
// container.TreeTarMaxEntries to guard against tar-bomb payloads.
//
// tar.Reader.Next() skips unread bytes automatically, so skipped entries do
// not require a separate drain step.
//
// Pure function: no I/O beyond the in-memory tar parse/rebuild; unit-tested
// without Docker in tree_at_test.go.
func rerootTarEntries(in []byte, stripPrefix, addPrefix string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(in))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("container/docker: rerootTarEntries: read tar: %w", err)
		}

		name := hdr.Name

		// Strip prefix; entries that lack it (including the root dir sentinel
		// Docker emits, e.g. "projects/") are skipped. tar.Reader.Next drains
		// unread body bytes automatically before advancing.
		if stripPrefix != "" {
			if !strings.HasPrefix(name, stripPrefix) {
				continue
			}
			name = strings.TrimPrefix(name, stripPrefix)
		}

		// Empty after stripping = root sentinel; also skip lone ".".
		if name == "" || name == "." {
			continue
		}

		// Validate: path.Clean strips trailing slash and detects escapes.
		clean := path.Clean(name)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("container/docker: rerootTarEntries: unsafe path %q rejected (path escape)", hdr.Name)
		}

		// Reconstruct name with addPrefix; restore trailing "/" for dirs.
		if addPrefix != "" {
			name = addPrefix + clean
		} else {
			name = clean
		}
		if hdr.Typeflag == tar.TypeDir && !strings.HasSuffix(name, "/") {
			name += "/"
		}

		entries++
		if entries > container.TreeTarMaxEntries {
			return nil, fmt.Errorf("container/docker: rerootTarEntries: entry count %d exceeds cap %d (possible zip-bomb)", entries, container.TreeTarMaxEntries)
		}

		newHdr := *hdr
		newHdr.Name = name
		if err := tw.WriteHeader(&newHdr); err != nil {
			return nil, fmt.Errorf("container/docker: rerootTarEntries: write header %q: %w", name, err)
		}
		// io.Copy copies hdr.Size bytes for regular files; 0 bytes for
		// dirs/symlinks — correct in both cases.
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, fmt.Errorf("container/docker: rerootTarEntries: copy body %q: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("container/docker: rerootTarEntries: close: %w", err)
	}
	return buf.Bytes(), nil
}

// ReadTreeAt captures the subtree rooted at dir inside the container as a
// gzip-tar whose entries are paths relative to dir (no leading basename
// component). Matches the wire format produced by the Fake and native backends.
//
// CopyFromContainer returns a plain-tar stream where all entries are prefixed
// with basename(dir)+"/"; rerootTarEntries strips that prefix before the
// archive is gzip-compressed for the caller.
//
// The handle is validated first (mirrors file_at.go's lock-lookup order). The
// Docker daemon surfaces a missing dir as an error from CopyFromContainer.
func (b *Backend) ReadTreeAt(ctx context.Context, h container.Handle, dir string) ([]byte, error) {
	r, err := b.lookupRegistered(ctx, "ReadTreeAt", h)
	if err != nil {
		return nil, err
	}
	dockerID, err := b.resolveContainerID(ctx, h, r)
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: %w", err)
	}

	cleanDir := path.Clean(dir)

	rc, _, err := b.cli.CopyFromContainer(ctx, dockerID, cleanDir)
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: CopyFromContainer %q: %w", cleanDir, err)
	}
	defer func() { _ = rc.Close() }()

	// Drain the plain-tar stream Docker returns into memory.
	rawTar, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: read tar stream %q: %w", cleanDir, err)
	}

	// Strip the basename(dir)+"/" prefix Docker adds to all entries so the
	// returned archive is relative-to-dir per the Fake + native contract.
	stripPrefix := path.Base(cleanDir) + "/"
	normalizedTar, err := rerootTarEntries(rawTar, stripPrefix, "")
	if err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: normalize entries: %w", err)
	}

	// Compress to gzip-tar — all ReadTreeAt callers receive gzip-tar.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := io.Copy(gw, bytes.NewReader(normalizedTar)); err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("container/docker: ReadTreeAt: gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteTreeAt extracts a gzip-tar (as produced by ReadTreeAt or BuildTreeTar)
// into dir inside the container, creating dir and its parents as needed.
// Existing files are overwritten. Entry paths in the archive must be relative
// to dir.
//
// Uses CopyToContainer with "/" as dstPath (the WriteFileAt + CopyTo
// convention): entry names are prefixed with strings.TrimPrefix(dir, "/")+"/",
// causing the Docker daemon to place them under dir. The moby layer
// auto-creates implied ancestor directories (go-archive.createImpliedDirectories).
//
// The handle is validated BEFORE any tar decoding (mirrors file_at.go's
// lock-lookup-proceed order). Caps: decompressed bytes ≤ container.TreeTarMaxBytes;
// entry count ≤ container.TreeTarMaxEntries (enforced inside rerootTarEntries).
func (b *Backend) WriteTreeAt(ctx context.Context, h container.Handle, dir string, tarGz []byte) error {
	// Validate handle BEFORE any tar work.
	r, err := b.lookupRegistered(ctx, "WriteTreeAt", h)
	if err != nil {
		return err
	}
	dockerID, err := b.resolveContainerID(ctx, h, r)
	if err != nil {
		return fmt.Errorf("container/docker: WriteTreeAt: %w", err)
	}

	// Decompress with byte cap (zip-bomb guard on the gzip layer).
	gr, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return fmt.Errorf("container/docker: WriteTreeAt: gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	cr := &treeAtCappedReader{r: gr, limit: container.TreeTarMaxBytes}
	rawTar, err := io.ReadAll(cr)
	if err != nil {
		return fmt.Errorf("container/docker: WriteTreeAt: decompress: %w", err)
	}

	// Build the full-path prefix for CopyToContainer("/").
	// E.g. dir="/work/.awf/projects" → addPrefix="work/.awf/projects/".
	cleanDir := path.Clean(dir)
	addPrefix := strings.TrimPrefix(cleanDir, "/") + "/"

	rerootedTar, err := rerootTarEntries(rawTar, "", addPrefix)
	if err != nil {
		return fmt.Errorf("container/docker: WriteTreeAt: reroot entries: %w", err)
	}

	if err := b.extractToContainer(ctx, dockerID, "/", bytes.NewReader(rerootedTar), ownByContainerUser); err != nil {
		return fmt.Errorf("container/docker: WriteTreeAt: CopyToContainer %q: %w", cleanDir, err)
	}
	return nil
}
