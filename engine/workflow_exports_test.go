package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestEvaluateWorkflowOutputsAgainstChildScope(t *testing.T) {
	wf := awfChildWorkflowWithOutput("child", "summary")
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("scan.workflow.summarize", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"summary": "child-only"},
	})

	got, err := evaluateWorkflowExports(rs, wf, "scan", map[string]any{"query": "CVE-1"}, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("evaluateWorkflowExports: %v", err)
	}
	if got.Outputs["summary"] != "child-only" {
		t.Fatalf("Outputs[summary] = %v, want child-only", got.Outputs["summary"])
	}
}

func TestEvaluateWorkflowOutputsRejectsParentStepRef(t *testing.T) {
	wf := awfChildWorkflowWithOutput("child", "summary")
	wf.Outputs["summary"] = json.RawMessage(`"{{ step.parent.summary }}"`)
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("parent", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"summary": "parent-only"},
	})

	_, err := evaluateWorkflowExports(rs, wf, "scan", nil, state.NewInMemoryBlobs())
	if err == nil {
		t.Fatal("evaluateWorkflowExports succeeded, want child-scope unresolved ref error")
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Fatalf("error = %v, want mention parent ref", err)
	}
}

func TestEvaluateWorkflowOutputFilesAliasChildArtifacts(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	ref, err := blobs.Put([]byte("report bytes"))
	if err != nil {
		t.Fatalf("Blobs.Put: %v", err)
	}
	wf := awfChildWorkflowWithArtifactExport("child")
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("scan.workflow.final", NodeResult{
		Outcome: OutcomeOK,
		Files:   map[string]string{"/out/report.md": ref},
	})

	got, err := evaluateWorkflowExports(rs, wf, "scan", nil, blobs)
	if err != nil {
		t.Fatalf("evaluateWorkflowExports: %v", err)
	}
	if got.Files["report"] != ref {
		t.Fatalf("Files[report] = %q, want %q", got.Files["report"], ref)
	}
}

func TestValidateWorkflowArtifactExportRejectsDynamicRef(t *testing.T) {
	wf := awfChildWorkflowWithArtifactExport("child")
	wf.ArtifactExports["report"] = "{{ step.final.files.report }}"

	diags := ir.Validate(awfLoadedDefinitionForWorkflow(wf))
	if !awfHasDiagnostic(diags, "AWF1049", "output_files.report") {
		t.Fatalf("missing AWF1049 at output_files.report; got %+v", diags)
	}
}

func TestValidateWorkflowArtifactExportRejectsMissingChildArtifact(t *testing.T) {
	wf := awfChildWorkflowWithArtifactExport("child")
	wf.ArtifactExports["report"] = "step.final.files.missing"

	diags := ir.Validate(awfLoadedDefinitionForWorkflow(wf))
	if !awfHasDiagnostic(diags, "AWF1049", "output_files.report") {
		t.Fatalf("missing AWF1049 at output_files.report; got %+v", diags)
	}
}

func TestResolveCallArtifactRefByExportName(t *testing.T) {
	wf := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan"},
		},
	}
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("recon", NodeResult{
		Outcome: OutcomeOK,
		Files:   map[string]string{"report": "cas-report"},
	})
	scope := NewScope(rs, wf, "consume")

	got, err := resolveNamedArtifactRef(scope, wf, "recon", "report")
	if err != nil {
		t.Fatalf("resolveNamedArtifactRef: %v", err)
	}
	if got != "cas-report" {
		t.Fatalf("resolved ref = %q, want cas-report", got)
	}
}

func TestEvaluateWorkflowOutputsSchemaFailure(t *testing.T) {
	wf := awfChildWorkflowWithOutput("child", "summary")
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("scan.workflow.summarize", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"summary": 42},
	})

	_, err := evaluateWorkflowExports(rs, wf, "scan", nil, state.NewInMemoryBlobs())
	if err == nil {
		t.Fatal("evaluateWorkflowExports succeeded, want schema failure")
	}
	if !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("error = %v, want output_schema context", err)
	}
}

func TestEvaluateWorkflowOutputsSchemaFailureWhenOutputsEmpty(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]ir.TemplateValue
	}{
		{name: "nil", outputs: nil},
		{name: "empty", outputs: map[string]ir.TemplateValue{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := awfChildWorkflowWithOutput("child", "summary")
			wf.Outputs = tt.outputs
			rs := NewRunState("run-1", "digest-1", nil)

			_, err := evaluateWorkflowExports(rs, wf, "scan", nil, state.NewInMemoryBlobs())
			if err == nil {
				t.Fatal("evaluateWorkflowExports succeeded, want output_schema required-field failure")
			}
			if !strings.Contains(err.Error(), "output_schema") {
				t.Fatalf("error = %v, want output_schema context", err)
			}
		})
	}
}

func awfChildWorkflowWithOutput(id, field string) *ir.Workflow {
	return &ir.Workflow{
		ID:           id,
		Version:      1,
		Containers:   map[string]ir.Container{"c": {Image: "oci://x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		OutputSchema: awfStringObjectSchema(field),
		Outputs: map[string]ir.TemplateValue{
			field: json.RawMessage(`"{{ step.summarize.` + field + ` }}"`),
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:           "summarize",
				Container:    "c",
				Run:          `printf '{}' > "$AWF_OUTPUT"`,
				OutputSchema: awfStringObjectSchema(field),
			},
		},
	}
}

func awfChildWorkflowWithArtifactExport(id string) *ir.Workflow {
	return &ir.Workflow{
		ID:         id,
		Version:    1,
		Containers: map[string]ir.Container{"c": {Image: "oci://x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		ArtifactExports: ir.ArtifactExports{
			"report": "step.final.files.report",
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:          "final",
				Container:   "c",
				Run:         "true",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
			},
		},
	}
}

func awfStringObjectSchema(fields ...string) *ir.JSONSchema {
	props := map[string]any{}
	required := make([]any, 0, len(fields))
	for _, field := range fields {
		props[field] = map[string]any{"type": "string"}
		required = append(required, field)
	}
	schema := ir.JSONSchema{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	return &schema
}

func awfLoadedDefinitionForWorkflow(wf *ir.Workflow) *ir.LoadedDefinition {
	return &ir.LoadedDefinition{
		Workflow:     wf,
		WorkflowPath: "/repo/workflow.awf.yaml",
		ComposeFiles: map[string][]byte{},
		Assets:       map[string]ir.LoadedAsset{},
		Modules: map[string]*ir.LoadedModule{
			"": {
				ID:           "",
				Workflow:     wf,
				WorkflowPath: "/repo/workflow.awf.yaml",
				ComposeFiles: map[string][]byte{},
				Assets:       map[string]ir.LoadedAsset{},
			},
		},
	}
}

func awfHasDiagnostic(diags []ir.Diagnostic, code, path string) bool {
	for _, d := range diags {
		if d.Code == code && d.Path == path {
			return true
		}
	}
	return false
}
