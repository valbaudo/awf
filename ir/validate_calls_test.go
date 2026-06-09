package ir

import (
	"encoding/json"
	"testing"
)

func TestValidateUnknownCallTarget(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "root", Version: 1,
		Containers: map[string]Container{},
		Graph: NodeList{
			&CallStep{ID: "scan", Call: "missing"},
		},
	})

	assertErrorAt(t, Validate(ld), "AWF1040", "scan")
}

func TestValidateCallInputAgainstParentSchema(t *testing.T) {
	ld := loadedWithChild(
		&Workflow{
			ID: "root", Version: 1,
			Input:      objectSchema("cve"),
			Imports:    map[string]string{"scan": "modules/scan.awf.yaml"},
			Containers: map[string]Container{},
			Graph: NodeList{
				&CallStep{
					ID:   "scan",
					Call: "scan",
					Input: map[string]TemplateValue{
						"query": json.RawMessage(`"{{ input.missing }}"`),
					},
				},
			},
		},
		childWorkflowWithTypedOutput("child", "finding"),
	)

	assertErrorAt(t, Validate(ld), "AWF1041", "scan.input.query")
}

func TestValidateCallInputAgainstChildSchema(t *testing.T) {
	root := validCallingRoot()
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Input = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1041", "scan.input")
}

func TestValidateCallInputRejectsUnknownChildInputKey(t *testing.T) {
	root := validCallingRoot()
	root.Graph = NodeList{
		&CallStep{
			ID:   "scan",
			Call: "scan",
			Input: map[string]TemplateValue{
				"query": json.RawMessage(`"CVE-2026-0001"`),
				"extra": json.RawMessage(`"surprise"`),
			},
		},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Input = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1041", "scan.input.extra")
}

func TestValidateCallInputRejectsStaticTypeMismatch(t *testing.T) {
	root := validCallingRoot()
	root.Graph = NodeList{
		&CallStep{
			ID:   "scan",
			Call: "scan",
			Input: map[string]TemplateValue{
				"query": json.RawMessage(`42`),
			},
		},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Input = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1041", "scan.input.query")
}

func TestValidateCallInputRejectsNonEmptyInputWithoutChildSchema(t *testing.T) {
	root := validCallingRoot()
	root.Graph = NodeList{
		&CallStep{
			ID:   "scan",
			Call: "scan",
			Input: map[string]TemplateValue{
				"query": json.RawMessage(`"CVE-2026-0001"`),
			},
		},
	}
	ld := loadedWithChild(root, childWorkflowWithTypedOutput("child", "finding"))

	assertErrorAt(t, Validate(ld), "AWF1041", "scan.input")
}

func TestValidateCallInputAcceptsValidStaticInput(t *testing.T) {
	root := validCallingRoot()
	root.Graph = NodeList{
		&CallStep{
			ID:   "scan",
			Call: "scan",
			Input: map[string]TemplateValue{
				"query": json.RawMessage(`"CVE-2026-0001"`),
			},
		},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Input = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertNoErrorCode(t, Validate(ld), "AWF1041")
}

func TestValidateRejectsParentRefInsideChildExport(t *testing.T) {
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Outputs = map[string]TemplateValue{
		"finding": json.RawMessage(`"{{ step.parent.finding }}"`),
	}
	ld := loadedWithChild(validCallingRoot(), child)

	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF1042", "outputs.finding")
	if got := diagnosticFor(diags, "AWF1042", "outputs.finding"); got == nil || got.Source != "/repo/modules/scan.awf.yaml" {
		t.Fatalf("AWF1042 Source = %v, want child workflow path", got)
	}
}

func TestValidateCallProducerRefsUseChildExportSchema(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = append(root.Graph, &CodeStep{
		ID:        "use",
		Container: "c",
		Run:       `echo {{ step.scan.finding }}`,
	})
	ld := loadedWithChild(root, childWorkflowWithTypedOutput("child", "finding"))

	diags := Validate(ld)
	assertNoErrorCode(t, diags, "AWF3001")
	assertNoErrorCode(t, diags, "AWF1042")
}

func TestValidateRejectsMissingRequiredWorkflowOutputBinding(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = append(root.Graph, &CodeStep{
		ID:        "use",
		Container: "c",
		Run:       `echo {{ step.scan.finding }}`,
	})
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Outputs = nil
	ld := loadedWithChild(root, child)

	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF3001", "use.run")
	assertErrorAt(t, diags, "AWF1042", "outputs.finding")
	if got := diagnosticFor(diags, "AWF1042", "outputs.finding"); got == nil || got.Source != "/repo/modules/scan.awf.yaml" {
		t.Fatalf("AWF1042 Source = %v, want child workflow path", got)
	}
}

func TestValidateRejectsParentRefToUnboundOptionalWorkflowOutput(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = append(root.Graph, &CodeStep{
		ID:        "use",
		Container: "c",
		Run:       `echo {{ step.scan.summary }}`,
	})
	child := childWorkflowWithTypedOutput("child", "finding")
	child.OutputSchema = &JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"finding": map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"},
		},
		"required":             []any{"finding"},
		"additionalProperties": false,
	}
	child.Outputs = map[string]TemplateValue{
		"finding": json.RawMessage(`"{{ step.produce.finding }}"`),
	}
	ld := loadedWithChild(root, child)

	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF3001", "use.run")
	assertNoErrorCode(t, diags, "AWF1042")
}

func TestValidateRejectsUndeclaredWorkflowOutputKey(t *testing.T) {
	child := childWorkflowWithTypedOutput("child", "finding")
	child.OutputSchema = &JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"finding": map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
	child.Outputs = map[string]TemplateValue{
		"extra": json.RawMessage(`"{{ step.produce.finding }}"`),
	}
	ld := loadedWithChild(validCallingRoot(), child)

	assertErrorAt(t, Validate(ld), "AWF1042", "outputs.extra")
}

func TestValidateAcceptsRequiredWorkflowOutputBinding(t *testing.T) {
	ld := loadedWithChild(validCallingRoot(), childWorkflowWithTypedOutput("child", "finding"))

	assertNoErrorCode(t, Validate(ld), "AWF1042")
}

func TestValidateCallArtifactRefsUseChildExportFiles(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = append(root.Graph, &CodeStep{
		ID:         "use",
		Container:  "c",
		Run:        "cat /tmp/report",
		InputFiles: map[string]string{"/tmp/report": "step.scan.files.report"},
	})
	child := childWorkflowWithArtifactExport("child")
	ld := loadedWithChild(root, child)

	diags := Validate(ld)
	assertNoErrorCode(t, diags, "AWF3007")
	assertNoErrorCode(t, diags, "AWF1043")
}

func TestValidateChildSchemaRefUsesChildModuleAssets(t *testing.T) {
	child := childWorkflowWithArtifactExport("child")
	child.Assets = map[string]string{"childschema": "schema.json"}
	child.Graph = NodeList{
		&CodeStep{
			ID:        "pack",
			Container: "c",
			Run:       "true",
			OutputFiles: OutputFiles{{
				Name:      "report",
				Path:      "/out/report.json",
				Format:    "json",
				SchemaRef: "asset.childschema",
			}},
		},
	}
	ld := loadedWithChild(validCallingRoot(), child)
	ld.Modules["module-scan"].Assets = map[string]LoadedAsset{
		"childschema": singleFileAsset("childschema", `{"type":"object"}`),
	}

	assertNoErrorCode(t, Validate(ld), "AWF3009")
}

func TestValidateArtifactOnlyChildWorkflowCanBeCalledForFileRef(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = append(root.Graph, &CodeStep{
		ID:         "use",
		Container:  "c",
		Run:        "cat /tmp/report",
		InputFiles: map[string]string{"/tmp/report": "step.scan.files.report"},
	})
	child := childWorkflowWithArtifactExport("child")
	child.OutputSchema = nil
	child.Outputs = nil
	ld := loadedWithChild(root, child)

	diags := Validate(ld)
	assertNoErrorCode(t, diags, "AWF1042")
	assertNoErrorCode(t, diags, "AWF3007")
}

func TestValidateDiagnosticSourceForImportedWorkflow(t *testing.T) {
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Containers["c"] = Container{Image: "oci://x:latest"}
	ld := loadedWithChild(validCallingRoot(), child)

	diags := Validate(ld)
	got := diagnosticFor(diags, "AWF1007", "containers.c.image")
	if got == nil {
		t.Fatalf("missing AWF1007 in %+v", diags)
	}
	if got.Source != "/repo/modules/scan.awf.yaml" {
		t.Fatalf("Source = %q, want child workflow path", got.Source)
	}
}

func TestValidateRejectsDuplicateStepAndAggregateProductIDs(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "root", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CallStep{ID: "dup", Call: "scan"},
			&Map{
				ID:          "dup",
				Over:        Expr("{{ input.items }}"),
				As:          "item",
				Container:   "c",
				Concurrency: 1,
				Body:        NodeList{&CodeStep{ID: "body", Container: "c", Run: "true"}},
			},
		},
	})

	assertOneError(t, Validate(ld), "AWF1004")
}

func loadedWithChild(root, child *Workflow) *LoadedDefinition {
	return &LoadedDefinition{
		Workflow:     root,
		WorkflowPath: "/repo/root.awf.yaml",
		ComposeFiles: map[string][]byte{},
		Assets:       map[string]LoadedAsset{},
		Modules: map[string]*LoadedModule{
			"": {
				ID:           "",
				Workflow:     root,
				WorkflowPath: "/repo/root.awf.yaml",
				ComposeFiles: map[string][]byte{},
				Assets:       map[string]LoadedAsset{},
			},
			"module-scan": {
				ID:           "module-scan",
				Workflow:     child,
				WorkflowPath: "/repo/modules/scan.awf.yaml",
				ComposeFiles: map[string][]byte{},
				Assets:       map[string]LoadedAsset{},
			},
		},
		ImportEdges: []LoadedImportEdge{{
			ParentID:     "",
			ImportID:     "scan",
			DeclaredPath: "modules/scan.awf.yaml",
			ChildID:      "module-scan",
		}},
	}
}

func validCallingRoot() *Workflow {
	return &Workflow{
		ID: "root", Version: 1,
		Imports:    map[string]string{"scan": "modules/scan.awf.yaml"},
		Containers: map[string]Container{},
		Graph: NodeList{
			&CallStep{ID: "scan", Call: "scan"},
		},
	}
}

func childWorkflowWithTypedOutput(id, field string) *Workflow {
	return &Workflow{
		ID: id, Version: 1,
		Containers:   map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		OutputSchema: objectSchema(field),
		Outputs: map[string]TemplateValue{
			field: json.RawMessage(`"{{ step.produce.` + field + ` }}"`),
		},
		Graph: NodeList{
			&CodeStep{
				ID:           "produce",
				Container:    "c",
				Run:          `printf '{}' > "$AWF_OUTPUT"`,
				OutputSchema: objectSchema(field),
			},
		},
	}
}

func childWorkflowWithArtifactExport(id string) *Workflow {
	return &Workflow{
		ID: id, Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		ArtifactExports: ArtifactExports{
			"report": "step.pack.files.report",
		},
		Graph: NodeList{
			&CodeStep{
				ID:          "pack",
				Container:   "c",
				Run:         "true",
				OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.json"}},
			},
		},
	}
}

func objectSchema(fields ...string) *JSONSchema {
	props := map[string]any{}
	required := make([]any, 0, len(fields))
	for _, field := range fields {
		props[field] = map[string]any{"type": "string"}
		required = append(required, field)
	}
	schema := JSONSchema{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	return &schema
}

func singleFileAsset(id, body string) LoadedAsset {
	return LoadedAsset{
		ID: id,
		Files: []LoadedAssetFile{{
			Path:  ".",
			Bytes: []byte(body),
		}},
	}
}

func diagnosticFor(diags []Diagnostic, code, path string) *Diagnostic {
	for i := range diags {
		if diags[i].Code == code && diags[i].Path == path {
			return &diags[i]
		}
	}
	return nil
}
