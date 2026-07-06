package ir

import "testing"

// ---- output_artifact validation (AWF3014) ----

// TestValidateOutputArtifact_OnContainerAgent: output_artifact on a container-backed agent
// step with output_schema → no AWF3014 (F39: widened from containerless-only).
func TestValidateOutputArtifact_OnContainerAgent(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&AgentStep{ID: "gen", Container: "c", Uses: "anthropic/claude-code",
				OutputArtifact: "result", OutputSchema: &JSONSchema{"type": "object"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3014")
}

// TestValidateOutputArtifact_WithOutputFiles: output_artifact mutually exclusive with output_files → AWF3014.
func TestValidateOutputArtifact_WithOutputFiles(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "gen", Uses: "awf/llm",
				OutputArtifact: "result",
				OutputSchema:   &JSONSchema{"type": "object"},
				OutputFiles:    OutputFiles{{Name: "extra", Path: "/out/extra.json"}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3014", "gen")
}

// TestValidateOutputArtifact_OnContainerAgentWithOutputFiles: the mutual-exclusion-with-
// output_files rule still applies to a container-backed step now that output_artifact is
// valid there too (F39 widens WHERE it's allowed, not the co-presence rule).
func TestValidateOutputArtifact_OnContainerAgentWithOutputFiles(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&AgentStep{ID: "gen", Container: "c", Uses: "anthropic/claude-code",
				OutputArtifact: "result",
				OutputSchema:   &JSONSchema{"type": "object"},
				OutputFiles:    OutputFiles{{Name: "extra", Path: "/out/extra.json"}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3014", "gen")
}

// TestValidateOutputArtifact_NoOutputSchema: output_artifact requires output_schema → AWF3014.
func TestValidateOutputArtifact_NoOutputSchema(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "gen", Uses: "awf/llm", OutputArtifact: "result"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3014", "gen")
}

// TestValidateOutputArtifact_BadName: output_artifact name must match stepIDPattern → AWF3014.
func TestValidateOutputArtifact_BadName(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "gen", Uses: "awf/llm",
				OutputArtifact: "bad name!", OutputSchema: &JSONSchema{"type": "object"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3014", "gen")
}

// TestValidateOutputArtifact_Valid: a containerless agent step with a valid output_artifact → no AWF3014.
func TestValidateOutputArtifact_Valid(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "gen", Uses: "awf/llm",
				OutputArtifact: "result", OutputSchema: &JSONSchema{"type": "object"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3014")
}

func TestValidateOutputFileContractRequiresPath(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{Name: "summary", Format: "json"}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFileContractFormatValues(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{Name: "summary", Path: "/out/summary.txt", Format: "text"}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFileContractSchemaRequiresFormat(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{
					Name:   "summary",
					Path:   "/out/summary.json",
					Schema: &JSONSchema{"type": "object"},
				}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFileContractSchemaAndSchemaRefMutuallyExclusive(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"schema": "schema.json"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{
					Name:      "summary",
					Path:      "/out/summary.json",
					Format:    "json",
					Schema:    &JSONSchema{"type": "object"},
					SchemaRef: "asset.schema",
				}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFileContractSchemaRefMustNameDeclaredAsset(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"schema": "schema.json"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{
					Name:      "summary",
					Path:      "/out/summary.json",
					Format:    "json",
					SchemaRef: "asset.missing",
				}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFilesDuplicateLiteralPathsRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{
					{Name: "strict", Path: "/out/summary.json", Format: "json", Schema: &JSONSchema{"type": "object"}},
					{Name: "alias", Path: "/out/summary.json", Format: "json"},
				}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFilesNamedArtifactIDUsesIdentifierRules(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{Name: "../escape", Path: "/out/report.md"}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateOutputFileContractSchemaRefAssetSchemaWellFormed(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"schema": "schema.json"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{
					Name:      "summary",
					Path:      "/out/summary.json",
					Format:    "json",
					SchemaRef: "asset.schema",
				}}},
		},
	})
	ld.Assets = map[string]LoadedAsset{"schema": {
		ID:           "schema",
		DeclaredPath: "schema.json",
		Files: []LoadedAssetFile{{
			Path:  ".",
			Bytes: []byte(`{"type":"object","properties":{"status":{"type":"string"}}}`),
		}},
	}}
	assertNoErrorCode(t, Validate(ld), "AWF3009")
}

func TestValidateOutputFileContractSchemaRefAssetMustBeSingleFile(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"schemas": "schemas"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "produce", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{
					Name:      "summary",
					Path:      "/out/summary.json",
					Format:    "json",
					SchemaRef: "asset.schemas",
				}}},
		},
	})
	ld.Assets = map[string]LoadedAsset{"schemas": {
		ID:           "schemas",
		DeclaredPath: "schemas",
		IsDir:        true,
		Files:        []LoadedAssetFile{{Path: "a.json", Bytes: []byte(`{"type":"object"}`)}},
	}}
	assertErrorAt(t, Validate(ld), "AWF3009", "produce")
}

func TestValidateWorkflowInputFileContractFormatValues(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		InputFiles: WorkflowInputFiles{
			"report": {Format: "text"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1050", "input_files.report")
}

func TestValidateWorkflowInputFileContractSchemaRefUsesModuleAssets(t *testing.T) {
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Assets = map[string]string{"childschema": "schema.json"}
	child.InputFiles = WorkflowInputFiles{
		"report": {Format: "json", SchemaRef: "asset.childschema"},
	}
	ld := loadedWithChild(validCallingRoot(), child)
	ld.Modules["module-scan"].Assets = map[string]LoadedAsset{
		"childschema": singleFileAsset("childschema", `{"type":"object"}`),
	}

	assertNoErrorCode(t, Validate(ld), "AWF1050")
}
