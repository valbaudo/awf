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

	assertErrorAt(t, Validate(ld), "AWF1046", "scan")
}

func TestValidateCallInputAgainstParentSchema(t *testing.T) {
	ld := loadedWithChild(
		&Workflow{
			ID: "root", Version: 1,
			InputSchema: objectSchema("cve"),
			Imports:     map[string]string{"scan": "modules/scan.awf.yaml"},
			Containers:  map[string]Container{},
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

	assertErrorAt(t, Validate(ld), "AWF1047", "scan.input.query")
}

func TestValidateCallInputAgainstChildSchema(t *testing.T) {
	root := validCallingRoot()
	child := childWorkflowWithTypedOutput("child", "finding")
	child.InputSchema = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1047", "scan.input")
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
	child.InputSchema = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1047", "scan.input.extra")
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
	child.InputSchema = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertErrorAt(t, Validate(ld), "AWF1047", "scan.input.query")
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

	assertErrorAt(t, Validate(ld), "AWF1047", "scan.input")
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
	child.InputSchema = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertNoErrorCode(t, Validate(ld), "AWF1047")
}

func TestValidateRejectsParentRefInsideChildExport(t *testing.T) {
	child := childWorkflowWithTypedOutput("child", "finding")
	child.Outputs = map[string]TemplateValue{
		"finding": json.RawMessage(`"{{ step.parent.finding }}"`),
	}
	ld := loadedWithChild(validCallingRoot(), child)

	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF1048", "outputs.finding")
	if got := diagnosticFor(diags, "AWF1048", "outputs.finding"); got == nil || got.Source != "/repo/modules/scan.awf.yaml" {
		t.Fatalf("AWF1048 Source = %v, want child workflow path", got)
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
	assertNoErrorCode(t, diags, "AWF1048")
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
	assertErrorAt(t, diags, "AWF1048", "outputs.finding")
	if got := diagnosticFor(diags, "AWF1048", "outputs.finding"); got == nil || got.Source != "/repo/modules/scan.awf.yaml" {
		t.Fatalf("AWF1048 Source = %v, want child workflow path", got)
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
	assertNoErrorCode(t, diags, "AWF1048")
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

	assertErrorAt(t, Validate(ld), "AWF1048", "outputs.extra")
}

func TestValidateAcceptsRequiredWorkflowOutputBinding(t *testing.T) {
	ld := loadedWithChild(validCallingRoot(), childWorkflowWithTypedOutput("child", "finding"))

	assertNoErrorCode(t, Validate(ld), "AWF1048")
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
	assertNoErrorCode(t, diags, "AWF1049")
}

func TestValidateCallInputFilesAccepted(t *testing.T) {
	root, child := validCallInputFilesValidationPair()
	ld := loadedWithChild(root, child)

	assertNoError(t, Validate(ld))
}

func TestValidateCallInputFilesAcceptsWorkflowInputFileRef(t *testing.T) {
	root, child := validCallInputFilesValidationPair()
	root.InputFiles = WorkflowInputFiles{"report": {}}
	root.Graph = NodeList{
		&CallStep{ID: "scan", Call: "scan",
			InputFiles: map[string]string{"report": "input.files.report"}},
	}
	child.Graph = NodeList{
		&CodeStep{ID: "use", Container: "c", Run: "true",
			InputFiles: map[string]string{"/work/report.json": "input.files.report"}},
	}
	ld := loadedWithChild(root, child)

	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

func TestValidateCallInputFilesBindingShape(t *testing.T) {
	cases := []struct {
		name string
		edit func(root, child *Workflow)
		code string
		path string
	}{
		{
			name: "missing required name",
			edit: func(root, child *Workflow) {
				child.InputFiles = WorkflowInputFiles{"report": {}, "notes": {}}
			},
			code: "AWF1051", path: "scan.input_files.notes",
		},
		{
			name: "extra name",
			edit: func(root, child *Workflow) {
				root.Graph[1].(*CallStep).InputFiles["extra"] = "step.collect.files.report"
			},
			code: "AWF1051", path: "scan.input_files.extra",
		},
		{
			name: "templated rhs",
			edit: func(root, child *Workflow) {
				root.Graph[1].(*CallStep).InputFiles["report"] = "{{ step.collect.files.report }}"
			},
			code: "AWF3007", path: "scan.input_files.report",
		},
		{
			name: "binding when child declares none",
			edit: func(root, child *Workflow) {
				child.InputFiles = nil
			},
			code: "AWF1051", path: "scan.input_files.report",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, child := validCallInputFilesValidationPair()
			tc.edit(root, child)
			assertErrorAt(t, Validate(loadedWithChild(root, child)), tc.code, tc.path)
		})
	}
}

func TestValidateCallInputFilesRejectsLateProducer(t *testing.T) {
	root, child := validCallInputFilesValidationPair()
	root.Graph = NodeList{root.Graph[1], root.Graph[0]} // call before collect

	assertErrorAt(t, Validate(loadedWithChild(root, child)), "AWF3007", "scan.input_files.report")
}

func TestValidateCallInputFilesRejectsDirectoryAsset(t *testing.T) {
	root, child := validCallInputFilesValidationPair()
	root.Assets = map[string]string{"report_dir": "fixtures"}
	root.Graph[1].(*CallStep).InputFiles["report"] = "asset.report_dir"
	ld := loadedWithChild(root, child)
	ld.Modules[""].Assets = map[string]LoadedAsset{
		"report_dir": {
			ID: "report_dir", DeclaredPath: "fixtures", IsDir: true,
			Files: []LoadedAssetFile{{Path: "report.json", Bytes: []byte(`{"status":"ok"}`)}},
		},
	}

	assertErrorAt(t, Validate(ld), "AWF1051", "scan.input_files.report")
}

func TestValidateCallInputFilesAcceptsGatePromotedArtifact(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	root := validCallingRoot()
	root.Containers = awf5003Container()
	root.Graph = NodeList{
		&Gate{
			Generate:    NodeList{reconProducer()},
			Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "true", OutputSchema: schema}},
			Until:       Expr("{{ step.judge.exit_code == 0 }}"),
			MaxAttempts: 2,
		},
		&CallStep{ID: "scan", Call: "scan", InputFiles: map[string]string{"report": "step.recon.files.report"}},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.InputFiles = WorkflowInputFiles{"report": {}}

	assertNoError(t, Validate(loadedWithChild(root, child)))
}

func TestValidateCallInputFilesAcceptsReducePromotedArtifact(t *testing.T) {
	root := validCallingRoot()
	root.Containers = awf5003Container()
	root.InputSchema = &JSONSchema{"type": "object", "additionalProperties": false,
		"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}}
	root.Graph = NodeList{
		&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
			Body: NodeList{reduceBodyScan()},
			Reduce: &Reduce{Run: "true", Container: "c",
				OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}}}},
		&CallStep{ID: "send", Call: "scan", InputFiles: map[string]string{"report": "step.scan.files.report"}},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.InputFiles = WorkflowInputFiles{"report": {}}

	assertNoError(t, Validate(loadedWithChild(root, child)))
}

func TestValidateCallInputFilesRejectsQuorumReducerArtifact(t *testing.T) {
	root := validCallingRoot()
	root.Containers = awf5003Container()
	root.InputSchema = &JSONSchema{"type": "object", "additionalProperties": false,
		"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}}
	root.Graph = NodeList{
		&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
			Body: NodeList{&CodeStep{ID: "scan", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
					"required": []any{"concur"}, "properties": map[string]any{"concur": map[string]any{"type": "boolean"}}},
				OutputFiles: OutputFiles{{Name: "leaf", Path: "/out/leaf.txt"}}}},
			Reduce: &Reduce{Quorum: reduceRatio("2"), Field: "concur"}},
		&CallStep{ID: "send", Call: "scan", InputFiles: map[string]string{"report": "step.scan.files.leaf"}},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.InputFiles = WorkflowInputFiles{"report": {}}

	assertErrorAt(t, Validate(loadedWithChild(root, child)), "AWF3007", "send.input_files.report")
}

func TestTypedInputFilesObjectStillWorksInTemplates(t *testing.T) {
	wf := &Workflow{
		ID:      "typed-input-files-object",
		Version: 1,
		InputSchema: &JSONSchema{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"report": map[string]any{"type": "string"},
					},
				},
			},
		},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "use", Container: "c", Run: "./use {{ input.files.report }}"},
		},
	}
	assertNoErrorCode(t, Validate(makeLD(wf)), "AWF3001")
	assertNoErrorCode(t, Validate(makeLD(wf)), "AWF3007")
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
	assertNoErrorCode(t, diags, "AWF1048")
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
				Concurrency: intPtr(1),
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

func validCallInputFilesValidationPair() (*Workflow, *Workflow) {
	root := validCallingRoot()
	root.Containers = map[string]Container{
		"c": {Image: "oci://x@sha256:abc"},
	}
	root.Graph = NodeList{
		&CodeStep{ID: "collect", Container: "c", Run: "true",
			OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.json"}}},
		&CallStep{ID: "scan", Call: "scan",
			InputFiles: map[string]string{"report": "step.collect.files.report"}},
	}
	child := childWorkflowWithTypedOutput("child", "finding")
	child.InputFiles = WorkflowInputFiles{"report": {}}
	return root, child
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
