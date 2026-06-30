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

// firstOfDef builds a workflow with two independent if-branches (no else) each
// producing `answer`. Outputs binds `answer` via the first_of: directive.
// Inputs: {fast bool, thorough bool}.
func firstOfDef() *ir.LoadedDefinition {
	answerSchema := func() *ir.JSONSchema {
		return &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"answer"},
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
		}
	}
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.fast }}"),
				Then: ir.NodeList{
					&ir.CodeStep{ID: "quick", Container: "lab", Run: "./quick.sh", OutputSchema: answerSchema()},
				},
			},
			&ir.If{
				Cond: ir.Expr("{{ input.thorough }}"),
				Then: ir.NodeList{
					&ir.CodeStep{ID: "thorough", Container: "lab", Run: "./thorough.sh", OutputSchema: answerSchema()},
				},
			},
		},
		Outputs: map[string]ir.TemplateValue{
			"answer": json.RawMessage(`{"first_of":["{{ step.quick.answer }}","{{ step.thorough.answer }}"]}`),
		},
	}
	return &ir.LoadedDefinition{Workflow: wf}
}

// runFirstOf runs firstOfDef with the given input and evaluates exports.
// The run must complete with OutcomeOK; this helper fatals the test if not.
func runFirstOf(t *testing.T, input map[string]any) (engine.WorkflowExportResult, error) {
	t.Helper()
	def := firstOfDef()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./quick.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"answer":"quick-answer"}`)}, nil)
	fake.ProgramExec("./thorough.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"answer":"thorough-answer"}`)}, nil)
	rs.Input = input
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	return engine.EvaluateExports(rs, def.Workflow, "", nil, blobs)
}

// (d1) fast=false, thorough=true → first_of picks second (thorough).
func TestExports_FirstOfElseBranchWins(t *testing.T) {
	t.Parallel()
	res, err := runFirstOf(t, map[string]any{"fast": false, "thorough": true})
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if res.Outputs["answer"] != "thorough-answer" {
		t.Fatalf("answer=%v, want thorough-answer (thorough branch ran)", res.Outputs["answer"])
	}
}

// (d2) fast=true, thorough=false → first_of picks first (quick).
func TestExports_FirstOfThenBranchWins(t *testing.T) {
	t.Parallel()
	res, err := runFirstOf(t, map[string]any{"fast": true, "thorough": false})
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if res.Outputs["answer"] != "quick-answer" {
		t.Fatalf("answer=%v, want quick-answer (quick branch ran)", res.Outputs["answer"])
	}
}

// (d3) fast=false, thorough=false → all refs absent → answer key omitted.
func TestExports_FirstOfAllAbsentOmitsKey(t *testing.T) {
	t.Parallel()
	res, err := runFirstOf(t, map[string]any{"fast": false, "thorough": false})
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if _, present := res.Outputs["answer"]; present {
		t.Fatalf("answer should be OMITTED when both branches skipped; got %v", res.Outputs)
	}
}

// ifArtifactDef builds a workflow with an if-gated step `deep` that produces
// a named output_files artifact, and a workflow-level output_files entry
// aliasing it. When the cond (input.deep) is false the then-branch is skipped →
// the artifact ref is ABSENT (AWF4006) and the key must be OMITTED (not
// hard-failed), symmetric with outputs:.
func ifArtifactDef() *ir.LoadedDefinition {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.If{
				Cond: ir.Expr("{{ input.deep }}"),
				Then: ir.NodeList{
					&ir.CodeStep{
						ID:          "deep",
						Container:   "lab",
						Run:         "./deep.sh",
						OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
					},
				},
			},
		},
		ArtifactExports: ir.ArtifactExports{
			"report": "step.deep.files.report",
		},
	}
	return &ir.LoadedDefinition{Workflow: wf}
}

// (f) non-taken-branch → output_files key OMITTED, export OK.
func TestExports_ArtifactExportAbsentOnNonTakenBranch(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExecWithFiles("./deep.sh", container.ExecResult{ExitCode: 0}, nil, map[string][]byte{
		"/out/report.md": []byte("report bytes"),
	})
	rs.Input = map[string]any{"deep": false}
	def := ifArtifactDef()
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	res, err := engine.EvaluateExports(rs, def.Workflow, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports should succeed (absent artifact omit): %v", err)
	}
	if _, present := res.Files["report"]; present {
		t.Fatalf("report should be OMITTED when then-branch skipped; got %v", res.Files)
	}
}

// (g) taken-branch → output_files key PRESENT (positive control).
func TestExports_ArtifactExportPresentOnTakenBranch(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExecWithFiles("./deep.sh", container.ExecResult{ExitCode: 0}, nil, map[string][]byte{
		"/out/report.md": []byte("report bytes"),
	})
	rs.Input = map[string]any{"deep": true}
	def := ifArtifactDef()
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: oc=%v err=%v", oc, err)
	}
	res, err := engine.EvaluateExports(rs, def.Workflow, "", nil, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if res.Files["report"] == "" {
		t.Fatalf("report should be PRESENT when then-branch taken; got %v", res.Files)
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
