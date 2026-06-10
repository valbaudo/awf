package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

const RootModuleID = ""

func QualifiedAssetKey(moduleID, assetID string) string {
	if moduleID == RootModuleID {
		return assetID
	}
	return moduleID + "/" + assetID
}

func StoreRunStartedAssets(blobs state.Blobs, assets map[string]ir.LoadedAsset) (map[string]RunStartedAsset, error) {
	if len(assets) == 0 {
		return nil, nil
	}
	out := make(map[string]RunStartedAsset, len(assets))
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		stored, err := storeRunStartedAsset(blobs, id, assets[id])
		if err != nil {
			return nil, err
		}
		out[id] = stored
	}
	return out, nil
}

func StoreRunStartedAssetsForLoadedDefinition(blobs state.Blobs, ld *ir.LoadedDefinition) (map[string]RunStartedAsset, error) {
	out := map[string]RunStartedAsset{}
	if err := ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil || len(module.Assets) == 0 {
			return nil
		}
		ids := make([]string, 0, len(module.Assets))
		for id := range module.Assets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			key := QualifiedAssetKey(module.ID, id)
			stored, err := storeRunStartedAsset(blobs, key, module.Assets[id])
			if err != nil {
				return err
			}
			out[key] = stored
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func storeRunStartedAsset(blobs state.Blobs, key string, asset ir.LoadedAsset) (RunStartedAsset, error) {
	files := make([]RunStartedAssetFile, 0, len(asset.Files))
	for _, file := range asset.Files {
		ref, err := blobs.Put(file.Bytes)
		if err != nil {
			return RunStartedAsset{}, fmt.Errorf("put asset %q file %q: %w", key, file.Path, err)
		}
		sha := file.SHA256
		if sha == "" {
			sum := sha256.Sum256(file.Bytes)
			sha = hex.EncodeToString(sum[:])
		}
		files = append(files, RunStartedAssetFile{
			Path:   file.Path,
			Ref:    ref,
			Size:   file.Size,
			SHA256: sha,
		})
	}
	return RunStartedAsset{
		DeclaredPath: asset.DeclaredPath,
		IsDir:        asset.IsDir,
		Files:        files,
	}, nil
}
