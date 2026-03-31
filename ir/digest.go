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
// loader). Pure: does not modify w. The Digest field is excluded (`json:"-"`). composeFiles is
// nil if none. See (*Workflow).SetDigest for the in-place variant.
func (w *Workflow) ComputeDigest(composeFiles map[string][]byte) (string, error) {
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
		fmt.Fprintf(h, "\x00%d:%s\x00", len(p), p)
		h.Write(fh[:])
	}
	return digestScheme + hex.EncodeToString(h.Sum(nil)), nil
}

// SetDigest computes the workflow's content digest via ComputeDigest and stores it in w.Digest.
// Returns the computed value for convenience. Convenient for run-start / loader callers that
// want both the value and the field populated; ComputeDigest itself remains pure.
func (w *Workflow) SetDigest(composeFiles map[string][]byte) (string, error) {
	d, err := w.ComputeDigest(composeFiles)
	if err != nil {
		return "", err
	}
	w.Digest = d
	return d, nil
}
