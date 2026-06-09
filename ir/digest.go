package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gowebpki/jcs"
)

// digestScheme is the self-describing prefix. Bump (awf-d2) only on a deliberate canonicalization
// change. Unexported until an external consumer needs to compare it; promote when that day comes.
const digestScheme = "awf-d1:sha256:"

// ComputeDigest returns the self-describing content digest of the workflow, folding in the sha256
// of each referenced compose file (keyed by cleaned, workflow-relative path supplied by the
// loader) and each loaded asset file. Pure: does not modify w. The Digest field is excluded
// (`json:"-"`). composeFiles/assets are nil if none. See (*Workflow).SetDigest for the in-place
// variant.
func (w *Workflow) ComputeDigest(composeFiles map[string][]byte, assets map[string]LoadedAsset) (string, error) {
	raw, err := json.Marshal(w) // Node marshalers produce the key-presence shape; Digest is json:"-"
	if err != nil {
		return "", fmt.Errorf("marshal workflow: %w", err)
	}
	canon, err := jcs.Transform(raw) // RFC 8785: sorts keys — independent of Go field/map order
	if err != nil {
		return "", fmt.Errorf("jcs canonicalize: %w", err)
	}
	h := sha256.New()
	h.Write(canon)
	paths := make([]string, 0, len(composeFiles))
	for p := range composeFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fh := sha256.Sum256(composeFiles[p])
		// length-prefix + NUL sentinels: \x00<byteLen(p)>:<p>\x00 — prevents aliasing for any
		// path byte sequence (incl. paths containing ':' or digits) and separates the path from
		// the following 32-byte content hash without depending on the reader knowing the hash size.
		// Explicit `_, _ =` to satisfy errcheck (Fprintf returns an error that staticcheck
		// prefers we use here over Write([]byte(fmt.Sprintf(...))); hash.Hash.Write is
		// errcheck-whitelisted so the line below stays unannotated).
		_, _ = fmt.Fprintf(h, "\x00%d:%s\x00", len(p), p)
		h.Write(fh[:])
	}
	assetIDs := make([]string, 0, len(assets))
	for id := range assets {
		assetIDs = append(assetIDs, id)
	}
	sort.Strings(assetIDs)
	for _, id := range assetIDs {
		asset := assets[id]
		if asset.ID != id {
			return "", fmt.Errorf("asset %q loaded with id %q", id, asset.ID)
		}
		writeDigestFrame(h, "asset")
		writeDigestFrame(h, asset.ID)
		writeDigestFrame(h, asset.DeclaredPath)
		if asset.IsDir {
			writeDigestFrame(h, "dir")
		} else {
			writeDigestFrame(h, "file")
		}
		files := append([]LoadedAssetFile(nil), asset.Files...)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		for _, f := range files {
			writeDigestFrame(h, "asset-file")
			writeDigestFrame(h, f.Path)
			fh := sha256.Sum256(f.Bytes)
			h.Write(fh[:])
		}
	}
	return digestScheme + hex.EncodeToString(h.Sum(nil)), nil
}

// SetDigest computes the workflow's content digest via ComputeDigest and stores it in w.Digest.
// Returns the computed value for convenience. Convenient for run-start / loader callers that
// want both the value and the field populated; ComputeDigest itself remains pure.
func (w *Workflow) SetDigest(composeFiles map[string][]byte, assets map[string]LoadedAsset) (string, error) {
	d, err := w.ComputeDigest(composeFiles, assets)
	if err != nil {
		return "", err
	}
	w.Digest = d
	return d, nil
}

func writeDigestFrame(h hashWriter, s string) {
	_, _ = fmt.Fprintf(h, "\x00%d:%s\x00", len(s), s)
}

type hashWriter interface {
	Write([]byte) (int, error)
}
