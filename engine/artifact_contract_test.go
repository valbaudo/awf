package engine_test

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func TestValidateArtifactContractJSONLRejectsBlankLineWithLineNumber(t *testing.T) {
	err := engine.ValidateArtifactContract("/out/rows.jsonl", []byte("{\"id\":1}\n\n"), engine.OutputFileContract{Format: "jsonl"})
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want JSONL line-number failure", err)
	}
}

func TestValidateArtifactContractJSONLSchemaValidatesEachLine(t *testing.T) {
	schema := &ir.JSONSchema{
		"type":       "object",
		"required":   []any{"id"},
		"properties": map[string]any{"id": map[string]any{"type": "integer"}},
	}
	err := engine.ValidateArtifactContract("/out/rows.jsonl", []byte("{\"id\":1}\n{\"id\":\"bad\"}\n"), engine.OutputFileContract{
		Format: "jsonl",
		Schema: schema,
	})
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "schema validation") {
		t.Fatalf("err = %v, want schema validation failure on line 2", err)
	}
}

func TestLocalDispatcherOutputFileJSONContractRejectsInvalidCapture(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExecWithFiles("./produce.sh", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/summary.json": []byte("not json")})

	intent := engine.NodeIntent{
		Path: "produce",
		Node: &ir.CodeStep{ID: "produce", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:     "./produce.sh",
			OutputFiles: []string{"/out/summary.json"},
			OutputFileContracts: map[string]engine.OutputFileContract{
				"/out/summary.json": {Format: "json"},
			},
		},
	}
	dr, _, err := d.Run(t.Context(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want retryable_failure", dr.Outcome)
	}
	if dr.Err == nil || !strings.Contains(dr.Err.Error(), "artifact contract") {
		t.Fatalf("Err = %v, want artifact contract failure", dr.Err)
	}
	if len(dr.Files) != 0 {
		t.Fatalf("Files = %+v, want no captured files returned on invalid artifact", dr.Files)
	}
}
