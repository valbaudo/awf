package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func storeHarnessAssetSnapshots(blobs state.Blobs, assets map[string]ir.LoadedAsset) (map[string]engine.RunStartedAsset, error) {
	if len(assets) == 0 {
		return nil, nil
	}
	out := make(map[string]engine.RunStartedAsset, len(assets))
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		asset := assets[id]
		files := make([]engine.RunStartedAssetFile, 0, len(asset.Files))
		for _, file := range asset.Files {
			ref, err := blobs.Put(file.Bytes)
			if err != nil {
				return nil, fmt.Errorf("put asset %q file %q: %w", id, file.Path, err)
			}
			sum := sha256.Sum256(file.Bytes)
			files = append(files, engine.RunStartedAssetFile{
				Path:   file.Path,
				Ref:    ref,
				Size:   file.Size,
				SHA256: hex.EncodeToString(sum[:]),
			})
		}
		out[id] = engine.RunStartedAsset{
			DeclaredPath: asset.DeclaredPath,
			IsDir:        asset.IsDir,
			Files:        files,
		}
	}
	return out, nil
}
