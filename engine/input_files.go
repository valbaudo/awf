package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// errArtifactFetch marks a Blobs.Get failure during input_files staging. Unlike
// a ref-resolution error (author bug → permanent_failure), a fetch failure of a
// committed, content-addressed artifact is corruption/IO territory — the caller
// halts as an internal error (matching engine/fold.go + signal_step), so resume
// re-runs the uncommitted step and re-fetches. (In-run staging retry is a
// future enhancement; out of scope for SP1's local single-host CAS.) Caveat:
// this surfaces with the same exit class as an interpreter bug (the "" outcome),
// consistent with fold.go — a missing/corrupt committed blob is a data/infra
// fault, not a step outcome; we do not invent a new outcome class for it.
var errArtifactFetch = errors.New("engine: input_files artifact fetch failed")

// resolveInputFiles maps a step's input_files (container-path → artifact/asset ref)
// to staged bytes. Destination container paths are template-substituted against
// the consumer's scope before staging, matching output_files path templating.
// Ref errors (parse/undeclared/not-committed) return a plain error (caller →
// permanent_failure); a Blobs.Get failure is wrapped with errArtifactFetch
// (caller → internal halt). Sorted by dst for determinism.
func resolveInputFiles(in map[string]string, scope *Scope, wf *ir.Workflow, blobs state.Blobs, assets map[string]RunStartedAsset) ([]container.InputFile, error) {
	if len(in) == 0 {
		return nil, nil
	}
	dsts := make([]string, 0, len(in))
	for d := range in {
		dsts = append(dsts, d)
	}
	sort.Strings(dsts)
	expanded := make([]resolvedInputFile, 0, len(in))
	for _, dst := range dsts {
		resolvedDst, err := template.Substitute(dst, scope)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: substitute destination: %w", dst, err)
		}
		if !path.IsAbs(resolvedDst) || resolvedDst != path.Clean(resolvedDst) {
			return nil, fmt.Errorf("input_files[%s]: substituted destination %q must be an absolute, clean path (no '..' segment)", dst, resolvedDst)
		}
		rawRef := in[dst]
		if id, ok := template.ParseAssetRef(rawRef); ok {
			files, err := resolveAssetInputFiles(resolvedDst, rawRef, id, assets, blobs)
			if err != nil {
				return nil, fmt.Errorf("input_files[%s]: %w", dst, err)
			}
			expanded = append(expanded, files...)
			continue
		}
		id, name, ok := template.ParseArtifactRef(rawRef)
		if !ok {
			return nil, fmt.Errorf("input_files[%s]=%q: expected step.<id>.files.<name> or asset.<id>", dst, rawRef)
		}
		cas, err := resolveNamedArtifactRef(scope, wf, id, name)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: %w", dst, err)
		}
		b, err := blobs.Get(cas)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: %w (%v)", dst, errArtifactFetch, err)
		}
		expanded = append(expanded, resolvedInputFile{
			file:   container.InputFile{Path: resolvedDst, Content: b},
			source: rawRef,
		})
	}
	if err := rejectInputFilePathCollisions(expanded); err != nil {
		return nil, err
	}
	out := make([]container.InputFile, 0, len(expanded))
	for _, e := range expanded {
		out = append(out, e.file)
	}
	return out, nil
}

type resolvedInputFile struct {
	file   container.InputFile
	source string
}

func resolveAssetInputFiles(dst, rawRef, id string, assets map[string]RunStartedAsset, blobs state.Blobs) ([]resolvedInputFile, error) {
	asset, ok := assets[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s: asset %q was not recorded in run.started", errArtifactFetch, rawRef, id)
	}
	if !asset.IsDir {
		if len(asset.Files) != 1 || asset.Files[0].Path != "." {
			return nil, fmt.Errorf("%w: %s: file asset %q has invalid run-start manifest", errArtifactFetch, rawRef, id)
		}
		b, err := readRunStartedAssetFile(blobs, asset.Files[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawRef, err)
		}
		return []resolvedInputFile{{
			file:   container.InputFile{Path: dst, Content: b},
			source: rawRef,
		}}, nil
	}
	files := append([]RunStartedAssetFile(nil), asset.Files...)
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %s: directory asset %q has empty run-start manifest", errArtifactFetch, rawRef, id)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	out := make([]resolvedInputFile, 0, len(files))
	for _, f := range files {
		if !validAssetManifestPath(f.Path) {
			return nil, fmt.Errorf("%w: %s: directory asset %q has unsafe manifest path %q", errArtifactFetch, rawRef, id, f.Path)
		}
		b, err := readRunStartedAssetFile(blobs, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawRef, err)
		}
		out = append(out, resolvedInputFile{
			file:   container.InputFile{Path: path.Join(dst, f.Path), Content: b},
			source: rawRef,
		})
	}
	return out, nil
}

func validAssetManifestPath(p string) bool {
	return p != "" && p != "." && !path.IsAbs(p) && p == path.Clean(p) && p != ".." && !strings.HasPrefix(p, "../")
}

func readRunStartedAssetFile(blobs state.Blobs, file RunStartedAssetFile) ([]byte, error) {
	b, err := blobs.Get(file.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: asset file %q ref %q: %v", errArtifactFetch, file.Path, file.Ref, err)
	}
	if int64(len(b)) != file.Size {
		return nil, fmt.Errorf("%w: asset file %q ref %q size %d != recorded %d", errArtifactFetch, file.Path, file.Ref, len(b), file.Size)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if got != file.SHA256 {
		return nil, fmt.Errorf("%w: asset file %q ref %q sha256 %s != recorded %s", errArtifactFetch, file.Path, file.Ref, got, file.SHA256)
	}
	return b, nil
}

func rejectInputFilePathCollisions(files []resolvedInputFile) error {
	sort.Slice(files, func(i, j int) bool {
		if files[i].file.Path == files[j].file.Path {
			return files[i].source < files[j].source
		}
		return files[i].file.Path < files[j].file.Path
	})
	for i := 1; i < len(files); i++ {
		prev := files[i-1]
		cur := files[i]
		if prev.file.Path == cur.file.Path {
			return fmt.Errorf("input_files expanded paths collide: %s from %s and %s from %s", prev.file.Path, prev.source, cur.file.Path, cur.source)
		}
		if isInputFileAncestor(prev.file.Path, cur.file.Path) {
			return fmt.Errorf("input_files expanded paths collide: %s from %s is an ancestor of %s from %s", prev.file.Path, prev.source, cur.file.Path, cur.source)
		}
	}
	return nil
}

func isInputFileAncestor(parent, child string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}
