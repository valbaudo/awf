package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// strSummarySchema is the per-step output_schema for `deep`: a single required
// string field `summary`.
func strSummarySchema() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"summary"},
		"properties":           map[string]any{"summary": map[string]any{"type": "string"}},
	}
}

// ifOutputDef builds a workflow whose top-level output `summary` binds
// {{ step.deep.summary }}, where `deep` lives under if[0].then. When the cond
// (input.deep) is false the then-branch is skipped → the ref is ABSENT (AWF4006).
// requiredSummary toggles whether the workflow output_schema marks summary
// required (else it is optional).
func ifOutputDef(requiredSummary bool) *ir.LoadedDefinition {
	outSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"summary": map[string]any{"type": "string"}},
	}
	if requiredSummary {
		(*outSchema)["required"] = []any{"summary"}
	}
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{ID: "deep", Container: "lab", Run: "./deep.sh", OutputSchema: strSummarySchema()},
				},
			},
		},
		OutputSchema: outSchema,
		Outputs:      map[string]ir.TemplateValue{"summary": json.RawMessage(`"{{ step.deep.summary }}"`)},
	}
	return &ir.LoadedDefinition{Workflow: wf}
}

// runIfThenEvaluateExports runs def to completion with input {deep: <deep>},
// then evaluates the workflow's top-level exports against the folded RunState
// (ctxPath="" — the awf-outputs / top-level shape).
func runIfThenEvaluateExports(t *testing.T, def *ir.LoadedDefinition, deep bool) (engine.WorkflowExportResult, error) {
	t.Helper()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./deep.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"summary":"deep-value"}`)}, nil)
	rs.Input = map[string]any{"deep": deep}
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	return engine.EvaluateExports(rs, def.Workflow, "", nil, blobs)
}

// (a) taken-branch ref → value present.
func TestExports_TakenBranchRefPresent(t *testing.T) {
	t.Parallel()
	res, err := runIfThenEvaluateExports(t, ifOutputDef(true), true)
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if res.Outputs["summary"] != "deep-value" {
		t.Fatalf("Outputs[summary] = %v, want deep-value", res.Outputs["summary"])
	}
}

// (b) non-taken-branch ref bound to an OPTIONAL output_schema field → key
// omitted, export OK.
func TestExports_OmitsAbsentOptionalBinding(t *testing.T) {
	t.Parallel()
	res, err := runIfThenEvaluateExports(t, ifOutputDef(false), false)
	if err != nil {
		t.Fatalf("EvaluateExports should succeed (optional omit): %v", err)
	}
	if _, present := res.Outputs["summary"]; present {
		t.Fatalf("summary should be OMITTED when then-branch skipped; got %v", res.Outputs)
	}
}

// (c) non-taken-branch ref bound to a REQUIRED field → ValidateOutputMap fails.
func TestExports_AbsentRequiredFieldFails(t *testing.T) {
	t.Parallel()
	_, err := runIfThenEvaluateExports(t, ifOutputDef(true), false)
	if err == nil {
		t.Fatal("absent binding to a REQUIRED output_schema field must fail validation")
	}
	if !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("err = %v, want output_schema validation context", err)
	}
}

// (e) AWF4006 in a NON-outputs position (a later code step's run: substitutes
// the under-if ref) → that node is permanent_failure with code AWF4006, NOT
// silently substituted.
func TestRunCodeRunAbsentIfRefIsPermanent(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./deep.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"summary":"x"}`)}, nil)
	rs.Input = map[string]any{"deep": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.deep }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "deep", Container: "lab", Run: "./deep.sh", OutputSchema: strSummarySchema()},
			},
		},
		&ir.CodeStep{ID: "consume", Container: "lab", Run: `echo {{ step.deep.summary }}`},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4006") {
		t.Errorf("err = %v, want AWF4006", err)
	}
	// The under-if ref must NOT have been silently substituted: `deep` never
	// committed, so `consume` never dispatched.
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls = %d, want 0 (consume must fail at substitution, never dispatch)", len(fake.Calls))
	}
}
