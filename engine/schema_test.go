package engine_test

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func TestValidateAgainstSchemaHappyPath(t *testing.T) {
	t.Parallel()
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	got, err := engine.ValidateAgainstSchema([]byte(`{"k":"v"}`), schema)
	if err != nil {
		t.Fatalf("ValidateAgainstSchema: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf("decoded[k] = %v, want %q", got["k"], "v")
	}
}

func TestValidateAgainstSchemaRejectsBadType(t *testing.T) {
	t.Parallel()
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	_, err := engine.ValidateAgainstSchema([]byte(`{"k":42}`), schema)
	if err == nil {
		t.Fatal("err = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "schema validation") {
		t.Errorf("err = %v, want mention of 'schema validation'", err)
	}
}

func TestValidateAgainstSchemaRejectsBadJSON(t *testing.T) {
	t.Parallel()
	schema := &ir.JSONSchema{"type": "object"}
	_, err := engine.ValidateAgainstSchema([]byte(`not json`), schema)
	if err == nil {
		t.Fatal("err = nil, want decode error")
	}
}
