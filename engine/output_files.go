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
	if of.Format != "" && of.Format != "json" && of.Format != "jsonl" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: format must be json or jsonl", of.Name)
	}
	if (of.Schema != nil || of.SchemaRef != "") && of.Format == "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema requires format json or jsonl", of.Name)
	}
	if of.Schema != nil && of.SchemaRef != "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema and schema_ref are mutually exclusive", of.Name)
	}
	contract := OutputFileContract{Format: of.Format, Schema: of.Schema}
	if of.Schema != nil {
		if _, err := compileJSONSchema(of.Schema); err != nil {
			return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema: %w", of.Name, err)
		}
	}
	if of.SchemaRef == "" {
		return contract, true, nil
	}
	id, ok := template.ParseAssetRef(of.SchemaRef)
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref must be asset.<id>", of.Name)
	}
	key := QualifiedAssetKey(moduleID, id)
	asset, ok := assets[key]
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("%w: output_files.%s: schema_ref asset %q was not recorded in run.started", errArtifactFetch, of.Name, key)
	}
	if asset.IsDir || len(asset.Files) != 1 || asset.Files[0].Path != "." {
		return OutputFileContract{}, false, fmt.Errorf("%w: output_files.%s: schema_ref asset %q has invalid run-start manifest", errArtifactFetch, of.Name, key)
	}
	raw, err := readRunStartedAssetFile(blobs, asset.Files[0])
	if err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: %w", of.Name, err)
	}
	var schema ir.JSONSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref asset %q is not JSON: %w", of.Name, key, err)
	}
	if _, err := compileJSONSchema(&schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref asset %q: %w", of.Name, key, err)
	}
	contract.Schema = &schema
	return contract, true, nil
}
