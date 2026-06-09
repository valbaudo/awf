package ir

import (
	"encoding/json"

	"github.com/valbaudo/awf/template"
)

func validateOutputFiles(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	WalkNodes(ld.Workflow.Graph, "", func(n Node, nodePath string) {
		switch s := n.(type) {
		case *CodeStep:
			validateOutputFileContracts(ld, c, nodePath, s.OutputFiles)
		case *AgentStep:
			validateOutputFileContracts(ld, c, nodePath, s.OutputFiles)
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
		if of.Format != "" && of.Format != "json" && of.Format != "jsonl" {
			c.errf(nodePath, "AWF3009", "output_files."+of.Name+": format must be json or jsonl")
		}
		if (of.Schema != nil || of.SchemaRef != "") && of.Format == "" {
			c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema requires format json or jsonl")
		}
		if of.Schema != nil && of.SchemaRef != "" {
			c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema and schema_ref are mutually exclusive")
		}
		if of.Schema != nil {
			checkSchemaWellFormed(*of.Schema, nodePath+".output_files."+of.Name+".schema", c)
		}
		if of.SchemaRef != "" {
			validateOutputFileSchemaRef(ld, c, nodePath, of)
		}
	}
}

func validateOutputFileSchemaRef(ld *LoadedDefinition, c *collector, nodePath string, of OutputFile) {
	id, ok := template.ParseAssetRef(of.SchemaRef)
	if !ok {
		c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema_ref must be asset.<id>")
		return
	}
	if _, declared := ld.Workflow.Assets[id]; !declared {
		c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema_ref asset "+id+" is not declared")
		return
	}
	asset, loaded := ld.Assets[id]
	if !loaded {
		return
	}
	if asset.IsDir || len(asset.Files) != 1 || asset.Files[0].Path != "." {
		c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema_ref asset "+id+" must be a single file")
		return
	}
	var schema JSONSchema
	if err := json.Unmarshal(asset.Files[0].Bytes, &schema); err != nil {
		c.errf(nodePath, "AWF3009", "output_files."+of.Name+": schema_ref asset "+id+" is not JSON")
		return
	}
	checkSchemaWellFormed(schema, nodePath+".output_files."+of.Name+".schema_ref", c)
}
