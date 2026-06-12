package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gowebpki/jcs"
)

// DigestScheme is the self-describing content-address prefix shared by workflow
// digests and state blob refs. Bump (awf-d2) only on a deliberate
// canonicalization change; state.blobRefPrefix and
// container/docker.snapshotBlobScheme are pinned equal to this by tests.
const DigestScheme = "awf-d1:sha256:"

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
	return DigestScheme + hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeDigest returns the content digest of the fully loaded definition. For a root-only
// definition it delegates exactly to Workflow.ComputeDigest to preserve the single-workflow digest
// contract. When imports are present, it folds the root workflow digest, each imported module's
// workflow digest, and the import edges into one deterministic loaded-definition digest.
func (ld *LoadedDefinition) ComputeDigest() (string, error) {
	if !ld.hasImportedModules() && len(ld.ImportEdges) == 0 {
		return ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
	}

	root := ld.Root()
	if root == nil || root.Workflow == nil {
		return "", fmt.Errorf("loaded definition missing root workflow")
	}
	rootDigest, err := ld.computeModuleWorkflowDigest(root)
	if err != nil {
		return "", fmt.Errorf("compute root workflow digest: %w", err)
	}

	h := sha256.New()
	writeDigestFrame(h, "loaded-definition-v1")
	writeDigestFrame(h, "root")
	writeDigestFrame(h, rootDigest)

	moduleIDs := make([]string, 0, len(ld.Modules))
	for id := range ld.Modules {
		if id != "" {
			moduleIDs = append(moduleIDs, id)
		}
	}
	sort.Strings(moduleIDs)
	for _, id := range moduleIDs {
		module := ld.Modules[id]
		if module == nil || module.Workflow == nil {
			return "", fmt.Errorf("loaded module %q missing workflow", id)
		}
		moduleDigest, err := ld.computeModuleWorkflowDigest(module)
		if err != nil {
			return "", fmt.Errorf("compute module %q workflow digest: %w", id, err)
		}
		writeDigestFrame(h, "module")
		writeDigestFrame(h, id)
		writeDigestFrame(h, moduleDigest)
	}

	if err := ld.WalkImportEdges(func(edge LoadedImportEdge) error {
		writeDigestFrame(h, "import-edge")
		writeDigestFrame(h, edge.ParentID)
		writeDigestFrame(h, edge.ImportID)
		writeDigestFrame(h, edge.DeclaredPath)
		writeDigestFrame(h, edge.ChildID)
		return nil
	}); err != nil {
		return "", err
	}

	return DigestScheme + hex.EncodeToString(h.Sum(nil)), nil
}

func (ld *LoadedDefinition) computeModuleWorkflowDigest(module *LoadedModule) (string, error) {
	wf := module.Workflow
	if len(wf.Imports) == 0 {
		return wf.ComputeDigest(module.ComposeFiles, module.Assets)
	}
	clone := *wf
	clone.Imports = ld.normalizedImportsFor(module.ID, wf.Imports)
	return (&clone).ComputeDigest(module.ComposeFiles, module.Assets)
}

func (ld *LoadedDefinition) normalizedImportsFor(moduleID string, imports map[string]string) map[string]string {
	out := make(map[string]string, len(imports))
	for id, declared := range imports {
		out[id] = declared
	}
	for _, edge := range ld.ImportEdges {
		if edge.ParentID == moduleID {
			out[edge.ImportID] = edge.DeclaredPath
		}
	}
	return out
}

func (ld *LoadedDefinition) hasImportedModules() bool {
	if ld == nil || len(ld.Modules) == 0 {
		return false
	}
	for id := range ld.Modules {
		if id != "" {
			return true
		}
	}
	return false
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
