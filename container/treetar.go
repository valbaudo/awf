package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// TreeTarMaxBytes is the default cumulative decompressed-bytes cap for
// ExtractTreeTar — mirrors native's snapshotDefaultMaxRestoreBytes (4 GiB).
const TreeTarMaxBytes int64 = 4 << 30

// TreeTarMaxEntries is the default entry-count cap for ExtractTreeTar — mirrors
// native's snapshotMaxEntries (1 000 000).
const TreeTarMaxEntries int = 1_000_000

// treeTarEntry is one entry in the archive being built.
type treeTarEntry struct {
	sortKey string // bare path, no trailing slash — used for stable ordering
	name    string // tar header Name (trailing slash for dirs)
	isDir   bool
	content []byte
}

// BuildTreeTar encodes files as a deterministic gzip-tar whose entries are
// sorted by path. Intermediate directory entries are synthesised for every
// parent component. All paths in the map are treated as relative (no leading
// slash). Headers carry zeroed mtime, uid, gid, uname, gname — identical
// content always produces identical bytes (the blob = content-hash invariant).
//
// BuildTreeTar does NOT validate that the caller's paths are free of ".." or
// absolute components; ExtractTreeTar enforces those guards on read.
func BuildTreeTar(files map[string][]byte) ([]byte, error) {
	// Collect all entries: synthesised dirs + regular files.
	dirSet := map[string]struct{}{}
	for p := range files {
		// Walk up the path, adding every parent directory.
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			dirSet[d] = struct{}{}
		}
	}

	entries := make([]treeTarEntry, 0, len(dirSet)+len(files))
	for d := range dirSet {
		entries = append(entries, treeTarEntry{sortKey: d, name: d + "/", isDir: true})
	}
	for p, content := range files {
		cp := make([]byte, len(content))
		copy(cp, content)
		entries = append(entries, treeTarEntry{sortKey: p, name: p, content: cp})
	}

	// Sort all entries together by their bare path so parent dirs always
	// precede their children (e.g. "b" < "b/c.jsonl" in string order).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].sortKey < entries[j].sortKey
	})

	var buf bytes.Buffer
	// gzip.NewWriter with default settings: zeroed Header (no Name, zero
	// ModTime) → deterministic bytes for identical content.
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, e := range entries {
		if e.isDir {
			hdr := &tar.Header{
				Name:     e.name,
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, fmt.Errorf("container: BuildTreeTar: write dir header %q: %w", e.name, err)
			}
		} else {
			hdr := &tar.Header{
				Name:     e.name,
				Typeflag: tar.TypeReg,
				Size:     int64(len(e.content)),
				Mode:     0o644,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, fmt.Errorf("container: BuildTreeTar: write file header %q: %w", e.name, err)
			}
			if _, err := tw.Write(e.content); err != nil {
				return nil, fmt.Errorf("container: BuildTreeTar: write file content %q: %w", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("container: BuildTreeTar: close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("container: BuildTreeTar: close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// ExtractTreeTar decompresses and decodes a gzip-tar produced by BuildTreeTar
// (or compatible tools) and returns a map of relative path → content for every
// regular file in the archive.
//
// Two zip-bomb / path-escape guards are enforced:
//   - maxBytes: cumulative decompressed bytes limit; triggers when exceeded.
//   - maxEntries: total entry count (dirs + files); triggers when exceeded.
//
// Any entry whose cleaned path starts with ".." or is absolute is rejected as a
// path-escape attempt. Only regular-file entries contribute to the returned map;
// directory entries are accepted (for tree structure) but not returned.
func ExtractTreeTar(tarGz []byte, maxBytes int64, maxEntries int) (map[string][]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return nil, fmt.Errorf("container: ExtractTreeTar: gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	// cappedReader counts every byte read from the decompressor — never trusts
	// tar header sizes. A single large file and many small files both trip the cap.
	cr := &treeTarCappedReader{r: gr, limit: maxBytes}
	tr := tar.NewReader(cr)

	out := map[string][]byte{}
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("container: ExtractTreeTar: tar: %w", err)
		}

		entries++
		if entries > maxEntries {
			return nil, fmt.Errorf("container: ExtractTreeTar: entry count %d exceeds cap %d (possible zip-bomb)", entries, maxEntries)
		}

		clean := path.Clean(hdr.Name)
		if isUnsafeTarPath(clean) {
			return nil, fmt.Errorf("container: ExtractTreeTar: unsafe path %q rejected (path escape)", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Accept — structural; not returned in the map.
		case tar.TypeReg:
			// io.ReadAll reads through the cappedReader, enforcing maxBytes.
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("container: ExtractTreeTar: read %q: %w", hdr.Name, err)
			}
			cp := make([]byte, len(content))
			copy(cp, content)
			out[clean] = cp
		default:
			return nil, fmt.Errorf("container: ExtractTreeTar: unsupported entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
	return out, nil
}

// isUnsafeTarPath reports whether the cleaned path is absolute or escapes the
// extraction root via "..".
func isUnsafeTarPath(clean string) bool {
	return path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../")
}

// treeTarCappedReader counts cumulative bytes read from the wrapped reader and
// returns an error once the limit is exceeded — the load-bearing zip-bomb guard
// (mirroring cappedReader in container/native/snapshot.go).
type treeTarCappedReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *treeTarCappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, fmt.Errorf("container: ExtractTreeTar: decompressed bytes %d exceed cap %d (possible zip-bomb)", c.n, c.limit)
	}
	return n, err
}
