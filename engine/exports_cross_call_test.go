package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// crossCallDef assembles a two-module LoadedDefinition: a root (parent) that
// imports a child workflow "scan". The child's `summary` output binds an
// under-if step `deep` (cond {{ input.deep }}); `summary` is OPTIONAL in the
// child output_schema, so when the branch is NOT taken the child legitimately
// OMITS summary (Part C C3). The parent binds its OWN outputs from parentOutputs
// (a `{{ step.mychild.<field> }}` ref) and declares parentOutSchemaProps as its
// output_schema properties (all optional). deep is passed false so the child's
// producing branch is skipped.
func crossCallDef(parentOutputs map[string]ir.TemplateValue, parentOutSchemaProps map[string]any) *ir.LoadedDefinition {
	childOutSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"summary": map[string]any{"type": "string"}},
	}
	childInSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"deep": map[string]any{"type": "boolean"}},
	}
	child := &ir.Workflow{
		ID:          "scan",
		Version:     1,
		InputSchema: childInSchema,
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		OutputSchema: childOutSchema,
		Outputs:      map[string]ir.TemplateValue{"summary": json.RawMessage(`"{{ step.deep.summary }}"`)},
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{ID: "deep", Container: "c", Run: "./deep.sh", OutputSchema: strSummarySchema()},
				},
			},
		},
	}
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{"scan": "scan.awf.yaml"},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           parentOutSchemaProps,
		},
		Outputs: parentOutputs,
		Graph: ir.NodeList{
			&ir.CallStep{ID: "mychild", Call: "scan", Input: map[string]ir.TemplateValue{
				"deep": json.RawMessage(`false`),
			}},
		},
	}
	return &ir.LoadedDefinition{
		Workflow:     root,
		WorkflowPath: "/repo/root.awf.yaml",
		ComposeFiles: map[string][]byte{},
		Modules: map[string]*ir.LoadedModule{
			"": {
				ID:           "",
				Workflow:     root,
				WorkflowPath: "/repo/root.awf.yaml",
				ComposeFiles: map[string][]byte{},
			},
			"mod-scan": {
				ID:           "mod-scan",
				Workflow:     child,
				WorkflowPath: "/repo/scan.awf.yaml",
				ComposeFiles: map[string][]byte{},
			},
		},
		ImportEdges: []ir.LoadedImportEdge{{ParentID: "", ImportID: "scan", ChildID: "mod-scan"}},
	}
}

// runCrossCall runs def to completion (deep=false → child omits summary) and
// evaluates the parent's top-level exports with the def-aware entry so the
// parent can classify a child-omitted output as ABSENT.
func runCrossCall(t *testing.T, def *ir.LoadedDefinition) (engine.WorkflowExportResult, error) {
	t.Helper()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	return engine.EvaluateExportsInDef(def, "", rs, def.Workflow, "", nil, blobs)
}

// C6 (a): a parent output bound to a CHILD-declared-but-omitted optional output
// resolves to ABSENT (AWF4006) so the parent OMITS the key — export OK, symmetric
// with the single-workflow if-branch omit (C3) but composed across a call.
func TestExports_CrossCall_OmitsChildOmittedOptional(t *testing.T) {
	t.Parallel()
	def := crossCallDef(
		map[string]ir.TemplateValue{"childSummary": json.RawMessage(`"{{ step.mychild.summary }}"`)},
		map[string]any{"childSummary": map[string]any{"type": "string"}},
	)
	res, err := runCrossCall(t, def)
	if err != nil {
		t.Fatalf("EvaluateExportsInDef should succeed (child-omitted optional → parent omits): %v", err)
	}
	if _, present := res.Outputs["childSummary"]; present {
		t.Fatalf("childSummary should be OMITTED when the child omitted summary; got %v", res.Outputs)
	}
}

// C6 (b) control: a parent output bound to a field the child NEVER declares in
// its output_schema is a genuine typo — it must still HARD-FAIL with AWF4002
// (EvalCodeRefUnresolved), NOT be silently omitted.
func TestExports_CrossCall_NeverDeclaredFieldStillFails(t *testing.T) {
	t.Parallel()
	def := crossCallDef(
		map[string]ir.TemplateValue{"childBogus": json.RawMessage(`"{{ step.mychild.notathing }}"`)},
		map[string]any{"childBogus": map[string]any{"type": "string"}},
	)
	_, err := runCrossCall(t, def)
	if err == nil {
		t.Fatal("a parent ref to a never-declared child field must hard-fail, not omit")
	}
	var ee *template.EvalError
	if !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
		t.Fatalf("err = %v, want AWF4002 (EvalCodeRefUnresolved)", err)
	}
}

// C6 (c) positive control: when the child's producing branch IS taken (deep=true)
// the child emits summary and the parent's binding resolves to the value.
func TestExports_CrossCall_TakenBranchForwardsValue(t *testing.T) {
	t.Parallel()
	def := crossCallDef(
		map[string]ir.TemplateValue{"childSummary": json.RawMessage(`"{{ step.mychild.summary }}"`)},
		map[string]any{"childSummary": map[string]any{"type": "string"}},
	)
	// Flip the call input to deep=true so the child's `deep` step runs.
	call := def.Workflow.Graph[0].(*ir.CallStep)
	call.Input["deep"] = json.RawMessage(`true`)

	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./deep.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"summary":"deep-value"}`)}, nil)
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	res, err := engine.EvaluateExportsInDef(def, "", rs, def.Workflow, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExportsInDef: %v", err)
	}
	if res.Outputs["childSummary"] != "deep-value" {
		t.Fatalf("childSummary = %v, want deep-value", res.Outputs["childSummary"])
	}
}
