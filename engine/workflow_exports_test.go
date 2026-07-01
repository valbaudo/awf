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

	got, err := evaluateWorkflowExports(nil, "", rs, wf, "scan", map[string]any{"query": "CVE-1"}, state.NewInMemoryBlobs())
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

	_, err := evaluateWorkflowExports(nil, "", rs, wf, "scan", nil, state.NewInMemoryBlobs())
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

	got, err := evaluateWorkflowExports(nil, "", rs, wf, "scan", nil, blobs)
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

	_, err := evaluateWorkflowExports(nil, "", rs, wf, "scan", nil, state.NewInMemoryBlobs())
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

			_, err := evaluateWorkflowExports(nil, "", rs, wf, "scan", nil, state.NewInMemoryBlobs())
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

// TestEvaluateExportsResumeIsDeterministic pins the determinism/resume invariant:
// folding the committed log into a FRESH RunState and re-running EvaluateExports
// produces an IDENTICAL omit/value decision. Specifically, rs.Branches is
// reproduced from EventBranchTaken so the taken/non-taken branch distinction
// round-trips through the log faithfully (the invariant that makes AWF4006 absent
// detection safe on resume — it cannot flip between runs).
func TestEvaluateExportsResumeIsDeterministic(t *testing.T) {
	blobs := state.NewInMemoryBlobs()

	// Commit the typed-outputs blob that Fold will materialise from OutputsRef.
	outputsRaw, err := json.Marshal(map[string]any{"summary": "deep-value"})
	if err != nil {
		t.Fatalf("marshal outputs: %v", err)
	}
	outputsRef, err := blobs.Put(outputsRaw)
	if err != nil {
		t.Fatalf("blobs.Put outputs: %v", err)
	}

	// Workflow: if[0].then.deep → optional summary binding {{ step.deep.summary }}.
	// summary is optional in the workflow output_schema so the non-taken case would
	// omit it; here we exercise the taken case to confirm the value is reproduced.
	wf := &ir.Workflow{
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			// summary is optional — not in "required" — so a non-taken branch omits
			// the key rather than hard-failing (symmetric with exports_optionality_test.go).
			"properties": map[string]any{"summary": map[string]any{"type": "string"}},
		},
		Outputs: map[string]ir.TemplateValue{
			"summary": json.RawMessage(`"{{ step.deep.summary }}"`),
		},
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{
						ID:           "deep",
						Container:    "c0",
						Run:          "./deep.sh",
						OutputSchema: awfStringObjectSchema("summary"),
					},
				},
			},
		},
	}

	// Synthesise the committed event sequence:
	//   run.started → branch.taken(if[0],"then") → node.completed(if[0].then.deep)
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r1", WorkflowDigest: "d1"})},
		{Seq: 2, TS: fixedTS, Path: "if[0]", Type: EventBranchTaken,
			Data: marshalOrFatal(t, BranchTakenData{Which: "then"})},
		{Seq: 3, TS: fixedTS, Path: "if[0].then.deep", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", OutputsRef: outputsRef})},
	}

	// Fold 1 — simulate the end of the original run.
	rs1, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold (first): %v", err)
	}
	result1, err := EvaluateExports(rs1, wf, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports (first): %v", err)
	}

	// Fold 2 — simulate a fresh resume from the SAME log (determinism invariant).
	rs2, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold (second): %v", err)
	}
	result2, err := EvaluateExports(rs2, wf, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports (second): %v", err)
	}

	// Pin: Branches reproduced from EventBranchTaken on both folds.
	if rs1.Branches["if[0]"] != "then" {
		t.Errorf("rs1.Branches[if[0]] = %q, want then", rs1.Branches["if[0]"])
	}
	if rs2.Branches["if[0]"] != "then" {
		t.Errorf("rs2.Branches[if[0]] = %q, want then", rs2.Branches["if[0]"])
	}

	// Pin: EvaluateExports produces identical results across both folds.
	if result1.Outputs["summary"] != result2.Outputs["summary"] {
		t.Errorf("determinism violated: fold1=%v fold2=%v", result1.Outputs["summary"], result2.Outputs["summary"])
	}
	if v := result1.Outputs["summary"]; v != "deep-value" {
		t.Errorf("Outputs[summary] = %v, want deep-value", v)
	}
}

// TestEvaluateExportsResumeOmitIsDeterministic pins the ABSENT/omit half of the
// determinism invariant: when the log records branch.taken(if[0],"else") and NO
// node.completed for the under-if step "deep", both folds reproduce
// Branches["if[0]"]=="else" → the {{ step.deep.summary }} binding resolves to
// AWF4006 → the optional summary key is OMITTED. This proves resume reproduces
// the omit DECISION, not just the value path (the sister test above).
func TestEvaluateExportsResumeOmitIsDeterministic(t *testing.T) {
	blobs := state.NewInMemoryBlobs()

	// summary is OPTIONAL (not in required) so the absent binding omits the key
	// rather than hard-failing.
	wf := &ir.Workflow{
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"summary": map[string]any{"type": "string"}},
		},
		Outputs: map[string]ir.TemplateValue{
			"summary": json.RawMessage(`"{{ step.deep.summary }}"`),
		},
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{
						ID:           "deep",
						Container:    "c0",
						Run:          "./deep.sh",
						OutputSchema: awfStringObjectSchema("summary"),
					},
				},
			},
		},
	}

	// Committed sequence: run.started → branch.taken(if[0],"else"). The else
	// branch is empty, so NO node.completed for "deep" — it is absent (AWF4006).
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r1", WorkflowDigest: "d1"})},
		{Seq: 2, TS: fixedTS, Path: "if[0]", Type: EventBranchTaken,
			Data: marshalOrFatal(t, BranchTakenData{Which: "else"})},
	}

	// Two independent folds simulate original-run-end and fresh-resume.
	rs1, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold (first): %v", err)
	}
	result1, err := EvaluateExports(rs1, wf, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports (first) should succeed (optional omit): %v", err)
	}

	rs2, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold (second): %v", err)
	}
	result2, err := EvaluateExports(rs2, wf, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports (second) should succeed (optional omit): %v", err)
	}

	// Pin: Branches reproduced as "else" on both folds.
	if rs1.Branches["if[0]"] != "else" || rs2.Branches["if[0]"] != "else" {
		t.Errorf("Branches[if[0]] = (%q, %q), want (else, else)", rs1.Branches["if[0]"], rs2.Branches["if[0]"])
	}

	// Pin: BOTH folds OMIT the summary key (the ABSENT/omit decision is deterministic).
	if _, present := result1.Outputs["summary"]; present {
		t.Errorf("fold1: summary should be OMITTED when else-branch taken; got %v", result1.Outputs)
	}
	if _, present := result2.Outputs["summary"]; present {
		t.Errorf("fold2: summary should be OMITTED when else-branch taken; got %v", result2.Outputs)
	}
}

func TestEvaluateExportsTopLevel(t *testing.T) {
	// Top-level run: the producer is keyed at its BARE id "summarize" (no prefix),
	// ctxPath is "", input is nil. This is the shape cli/outputs.go uses.
	wf := awfChildWorkflowWithOutput("root", "summary")
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("summarize", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"summary": "top-level"},
	})

	got, err := EvaluateExports(rs, wf, "", nil, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if got.Outputs["summary"] != "top-level" {
		t.Fatalf("Outputs[summary] = %v, want top-level", got.Outputs["summary"])
	}
}
