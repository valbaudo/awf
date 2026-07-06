package ir

import (
	"encoding/json"
	"sort"

	"github.com/valbaudo/awf/template"
)

func validateOutputFiles(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	validateWorkflowInputFiles(ld, c)
	WalkNodes(ld.Workflow.Graph, "", func(n Node, nodePath string) {
		switch s := n.(type) {
		case *CodeStep:
			validateOutputFileContracts(ld, c, nodePath, s.OutputFiles)
		case *AgentStep:
			validateOutputFileContracts(ld, c, nodePath, s.OutputFiles)
			validateOutputArtifact(ld, c, nodePath, s)
		case *Map:
			if s.Reduce != nil {
				validateOutputFileContracts(ld, c, nodePath+".reduce", s.Reduce.OutputFiles)
			}
		}
	})
}

func validateOutputFileContracts(ld *LoadedDefinition, c *collector, nodePath string, ofs OutputFiles) {
	paths := map[string]string{}
	for _, of := range ofs {
		if of.Name != "" && !stepIDPattern.MatchString(of.Name) {
			c.errf(nodePath, "AWF3009", "output_files."+of.Name+": name must match "+stepIDPattern.String())
		}
		if of.Path != "" {
			if prev, ok := paths[of.Path]; ok {
				c.errf(nodePath, "AWF3009", "output_files."+of.Name+": duplicate path "+of.Path+" already declared by "+prev)
			} else {
				paths[of.Path] = of.Name
			}
		}
		hasContract := of.Format != "" || of.Schema != nil || of.SchemaRef != ""
		if !hasContract {
			continue
		}
		if of.Name == "" {
			c.errf(nodePath, "AWF3009", "output_files contract metadata is only valid on named output_files entries")
		}
		if of.Path == "" {
			c.errf(nodePath, "AWF3009", "output_files."+of.Name+": contract object requires path")
		}
		validateArtifactContractMetadata(ld, c, nodePath, "AWF3009", "output_files."+of.Name, nodePath+".output_files."+of.Name, ContractFromOutputFile(of))
	}
}

func validateWorkflowInputFiles(ld *LoadedDefinition, c *collector) {
	keys := make([]string, 0, len(ld.Workflow.InputFiles))
	for key := range ld.Workflow.InputFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := "input_files." + key
		if !stepIDPattern.MatchString(key) {
			c.errf(path, "AWF1050", "workflow input_files."+key+": name must match "+stepIDPattern.String())
			continue
		}
		validateArtifactContractMetadata(ld, c, path, "AWF1050", "input_files."+key, path, ld.Workflow.InputFiles[key])
	}
}

func validateArtifactContractMetadata(ld *LoadedDefinition, c *collector, nodePath, code, label, schemaPath string, contract ArtifactContract) {
	if contract.Format != "" && contract.Format != "json" && contract.Format != "jsonl" {
		c.errf(nodePath, code, label+": format must be json or jsonl")
	}
	if (contract.Schema != nil || contract.SchemaRef != "") && contract.Format == "" {
		c.errf(nodePath, code, label+": schema requires format json or jsonl")
	}
	if contract.Schema != nil && contract.SchemaRef != "" {
		c.errf(nodePath, code, label+": schema and schema_ref are mutually exclusive")
	}
	if contract.Schema != nil {
		checkSchemaWellFormed(*contract.Schema, schemaPath+".schema", c)
	}
	if contract.SchemaRef != "" {
		validateSchemaRefAsset(ld, c, nodePath, code, label, contract.SchemaRef, schemaPath+".schema_ref")
	}
}

// validateOutputArtifact enforces the output_artifact rules (AWF3014): valid on any agent
// step (container-backed or containerless) with output_schema; name matches
// stepIDPattern; mutually exclusive with output_files. All violations share code AWF3014.
func validateOutputArtifact(_ *LoadedDefinition, c *collector, nodePath string, s *AgentStep) {
	if s.OutputArtifact == "" {
		return
	}
	if !stepIDPattern.MatchString(s.OutputArtifact) {
		c.errf(nodePath, "AWF3014", "output_artifact: name must match "+stepIDPattern.String())
	}
	if len(s.OutputFiles) > 0 {
		c.errf(nodePath, "AWF3014", "output_artifact and output_files are mutually exclusive on the same step")
	}
	if s.OutputSchema == nil {
		c.errf(nodePath, "AWF3014", "output_artifact requires output_schema (the artifact is the serialized typed output)")
	}
}

func validateSchemaRefAsset(ld *LoadedDefinition, c *collector, nodePath, code, label, schemaRef, schemaPath string) {
	id, ok := template.ParseAssetRef(schemaRef)
	if !ok {
		c.errf(nodePath, code, label+": schema_ref must be asset.<id>")
		return
	}
	if _, declared := ld.Workflow.Assets[id]; !declared {
		c.errf(nodePath, code, label+": schema_ref asset "+id+" is not declared")
		return
	}
	asset, loaded := ld.Assets[id]
	if !loaded {
		return
	}
	if asset.IsDir || len(asset.Files) != 1 || asset.Files[0].Path != "." {
		c.errf(nodePath, code, label+": schema_ref asset "+id+" must be a single file")
		return
	}
	var schema JSONSchema
	if err := json.Unmarshal(asset.Files[0].Bytes, &schema); err != nil {
		c.errf(nodePath, code, label+": schema_ref asset "+id+" is not JSON")
		return
	}
	checkSchemaWellFormed(schema, schemaPath, c)
}
