package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// blobRefPrefix is the namespace prefix all sha256 blob refs carry. Matches the digest
	// namespace slice 1.1 picked for ir.Workflow.Digest, so refs and workflow digests share
	// one vocabulary the operator can parse with the same rule.
	blobRefPrefix = "awf-d1:sha256:"
	// blobHashHexLen is the hex length of a sha256 digest (256 bits / 4 bits per nibble).
	blobHashHexLen = 64
	// blobShardLen is the number of hex chars used for the on-disk first-level shard
	// directory. 2 hex == 256 shards, matching git's object layout — keeps any single
	// directory's entry count bounded even with many blobs.
	blobShardLen = 2
)

var (
	// ErrBadRef wraps every refusal to parse a ref (wrong prefix, wrong length, non-hex,
	// wrong algorithm). Callers use errors.Is to distinguish from missing / corruption.
	ErrBadRef = errors.New("state: malformed blob ref")
	// ErrCorruption fires when Get reads a file whose recomputed sha256 doesn't match the
	// ref's address. Detects silent disk corruption.
	ErrCorruption = errors.New("state: blob content does not match its address")
)

// Blobs is the content-addressed artifact store seam. One concrete impl (FSBlobs) + one
// fake (InMemoryBlobs). Hash is hidden behind the ref string so a future BLAKE3 impl can
// land additively (refs would be "awf-d1:blake3:<hex>" and parse via the same prefix split).
type Blobs interface {
	Put(content []byte) (ref string, err error)
	Get(ref string) ([]byte, error)
}

// FSBlobs is the filesystem-backed CAS — one file per content address, sharded by the
// first two hex chars of the address.
type FSBlobs struct {
	root string
}

// OpenBlobs prepares the blob store rooted at the given directory. Creates the root
// directory (and the `sha256/` subdirectory) if they don't already exist.
func OpenBlobs(root string) (*FSBlobs, error) {
	algoDir := filepath.Join(root, "sha256")
	if err := os.MkdirAll(algoDir, 0o755); err != nil {
		return nil, fmt.Errorf("state: prepare blob root %q: %w", root, err)
	}
	return &FSBlobs{root: root}, nil
}

// Put writes content to the store under its sha256 address and returns the ref string.
// Idempotent: putting the same bytes a second time is a no-op (atomic rename onto the
// existing file produces an identical file). Atomic on POSIX (same-directory rename).
func (b *FSBlobs) Put(content []byte) (string, error) {
	ref := RefFor(content)
	hashHex := strings.TrimPrefix(ref, blobRefPrefix)
	shardDir := filepath.Join(b.root, "sha256", hashHex[:blobShardLen])
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		return "", fmt.Errorf("state: prepare shard %q: %w", shardDir, err)
	}
	finalPath := filepath.Join(shardDir, hashHex)
	// Fast path: file already exists (dedup). os.Stat is cheap; if it exists, the content
	// is by definition identical (sha256 collision resistance). No new directory entry is
	// created, so no syncDir needed.
	if _, err := os.Stat(finalPath); err == nil {
		return ref, nil
	}
	// Commit the shard subdirectory's creation (if it was new) before writing the blob file.
	if err := syncDir(filepath.Join(b.root, "sha256")); err != nil {
		return "", err
	}

	// Atomic write: tmp file in the same shard directory → fsync → rename.
	tmp, err := os.CreateTemp(shardDir, hashHex+".tmp.*")
	if err != nil {
		return "", fmt.Errorf("state: create tmp for %s: %w", ref, err)
	}
	tmpPath := tmp.Name()
	if _, werr := tmp.Write(content); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("state: write tmp for %s: %w", ref, werr)
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("state: sync tmp for %s: %w", ref, serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("state: close tmp for %s: %w", ref, cerr)
	}
	if rerr := os.Rename(tmpPath, finalPath); rerr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("state: rename tmp→final for %s: %w", ref, rerr)
	}
	if err := syncDir(shardDir); err != nil {
		return "", fmt.Errorf("state: sync shard dir after put %s: %w", ref, err)
	}
	return ref, nil
}

// Get reads the blob at ref and re-verifies its sha256 against the ref's address. Returns
// ErrBadRef for malformed refs, ErrCorruption for content/address mismatch, or a wrapped
// fs.ErrNotExist for refs the caller didn't Put.
func (b *FSBlobs) Get(ref string) ([]byte, error) {
	hashHex, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(b.root, "sha256", hashHex[:blobShardLen], hashHex)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("state: open blob %s: %w", ref, err)
	}
	defer func() { _ = f.Close() }() // read-only handle; Close error not meaningful here
	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("state: read blob %s: %w", ref, err)
	}
	got := sha256.Sum256(content)
	if hex.EncodeToString(got[:]) != hashHex {
		return nil, fmt.Errorf("%w: ref=%s, file content sha256=%s",
			ErrCorruption, ref, hex.EncodeToString(got[:]))
	}
	return content, nil
}

// RefFor returns the ref string a Put of content would produce. Pure; no I/O. Callers
// (the engine, when building events that point to soon-to-be-put payloads) can pre-compute.
func RefFor(content []byte) string {
	h := sha256.Sum256(content)
	return blobRefPrefix + hex.EncodeToString(h[:])
}

// parseRef validates a ref string and returns the hex portion. Rejects:
//   - missing/wrong prefix (must be exactly "awf-d1:sha256:");
//   - wrong hex length (must be exactly 64);
//   - non-hex characters (must match [0-9a-f]+);
//   - uppercase hex (RefFor only ever emits lowercase; accepting uppercase would let
//     a same-content-different-case ref miss the on-disk lowercase file and fall through
//     as fs.ErrNotExist, which is a confusing observable for an authoring mistake).
//
// Strict validation is required because the hex flows straight into a file path —
// a `..` or `/` in the ref must never reach the filesystem.
func parseRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, blobRefPrefix) {
		return "", fmt.Errorf("%w: missing prefix %q in %q", ErrBadRef, blobRefPrefix, ref)
	}
	rest := ref[len(blobRefPrefix):]
	if len(rest) != blobHashHexLen {
		return "", fmt.Errorf("%w: hex length %d, want %d in %q", ErrBadRef, len(rest), blobHashHexLen, ref)
	}
	if rest != strings.ToLower(rest) {
		return "", fmt.Errorf("%w: hex must be lowercase in %q", ErrBadRef, ref)
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return "", fmt.Errorf("%w: invalid hex in %q: %v", ErrBadRef, ref, err)
	}
	return rest, nil
}

// syncDir fsyncs a directory inode so that file-creation / rename operations within it
// become durable. Required after os.Rename and after creating a new directory whose parent
// dir's inode must reflect the new entry on next open. On Windows (which AWF doesn't target
// per CLAUDE.md), opening a directory for write would fail; we don't reach this code path.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("state: open dir %q for sync: %w", path, err)
	}
	if serr := d.Sync(); serr != nil {
		_ = d.Close()
		return fmt.Errorf("state: sync dir %q: %w", path, serr)
	}
	if cerr := d.Close(); cerr != nil {
		return fmt.Errorf("state: close dir %q after sync: %w", path, cerr)
	}
	return nil
}
