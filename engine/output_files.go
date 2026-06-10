package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// resolveOutputFiles template-substitutes each output_files path against the
// step's scope, returning capture paths plus any contract metadata keyed by the
// substituted capture path. output_files paths are templated exactly like run:
// and idempotency_key, so a path such as /work/records/{{ input.cve_id }}.json
// captures — and commits, PATH-keyed in commit.go — under the substituted name.
func resolveOutputFiles(ofs ir.OutputFiles, scope template.Scope, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs) ([]string, map[string]OutputFileContract, error) {
	if len(ofs) == 0 {
		return nil, nil, nil
	}
	paths := make([]string, 0, len(ofs))
	contracts := map[string]OutputFileContract{}
	seenPaths := map[string]string{}
	for _, of := range ofs {
		p, err := template.Substitute(of.Path, scope)
		if err != nil {
			return nil, nil, fmt.Errorf("output_files path %q: %w", of.Path, err)
		}
		if prev, ok := seenPaths[p]; ok {
			return nil, nil, fmt.Errorf("duplicate output_files path %q (declared by %s and %s after template substitution)", p, prev, of.Name)
		}
		seenPaths[p] = of.Name
		paths = append(paths, p)
		contract, hasContract, err := resolveOutputFileContract(of, moduleID, assets, blobs)
		if err != nil {
			return nil, nil, err
		}
		if hasContract {
			contracts[p] = contract
		}
	}
	if len(contracts) == 0 {
		contracts = nil
	}
	return paths, contracts, nil
}

func resolveOutputFileContract(of ir.OutputFile, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs) (OutputFileContract, bool, error) {
	if of.Format == "" && of.Schema == nil && of.SchemaRef == "" {
		return OutputFileContract{}, false, nil
	}
	if of.Path == "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: contract object requires path", of.Name)
	}
	return resolveArtifactContract("output_files."+of.Name, ir.ContractFromOutputFile(of), moduleID, assets, blobs)
}

func resolveArtifactContract(label string, contract ir.ArtifactContract, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs) (OutputFileContract, bool, error) {
	if contract.Format == "" && contract.Schema == nil && contract.SchemaRef == "" {
		return OutputFileContract{}, false, nil
	}
	if contract.Format != "" && contract.Format != "json" && contract.Format != "jsonl" {
		return OutputFileContract{}, false, fmt.Errorf("%s: format must be json or jsonl", label)
	}
	if (contract.Schema != nil || contract.SchemaRef != "") && contract.Format == "" {
		return OutputFileContract{}, false, fmt.Errorf("%s: schema requires format json or jsonl", label)
	}
	if contract.Schema != nil && contract.SchemaRef != "" {
		return OutputFileContract{}, false, fmt.Errorf("%s: schema and schema_ref are mutually exclusive", label)
	}
	out := OutputFileContract{Format: contract.Format, Schema: contract.Schema}
	if contract.Schema != nil {
		if _, err := compileJSONSchema(contract.Schema); err != nil {
			return OutputFileContract{}, false, fmt.Errorf("%s: schema: %w", label, err)
		}
	}
	if contract.SchemaRef == "" {
		return out, true, nil
	}
	id, ok := template.ParseAssetRef(contract.SchemaRef)
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("%s: schema_ref must be asset.<id>", label)
	}
	key := QualifiedAssetKey(moduleID, id)
	asset, ok := assets[key]
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("%w: %s: schema_ref asset %q was not recorded in run.started", errArtifactFetch, label, key)
	}
	if asset.IsDir || len(asset.Files) != 1 || asset.Files[0].Path != "." {
		return OutputFileContract{}, false, fmt.Errorf("%w: %s: schema_ref asset %q has invalid run-start manifest", errArtifactFetch, label, key)
	}
	raw, err := readRunStartedAssetFile(blobs, asset.Files[0])
	if err != nil {
		return OutputFileContract{}, false, fmt.Errorf("%s: %w", label, err)
	}
	var schema ir.JSONSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("%s: schema_ref asset %q is not JSON: %w", label, key, err)
	}
	if _, err := compileJSONSchema(&schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("%s: schema_ref asset %q: %w", label, key, err)
	}
	out.Schema = &schema
	return out, true, nil
}
