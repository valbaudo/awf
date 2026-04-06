package ir

import (
	"strings"
	"testing"
)

// Tests for the AWF2001/AWF2002 schema pass — see validate_schema.go.

func TestSchemaWellFormednessAWF2001(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "bad", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{
				ID: "a", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{"type": "not-a-type"}, // invalid metaschema
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF2001", "a.output_schema")
}

func TestSchemaFloorOneOfWarnsAWF2002(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"oneOf":                []any{},
					"required":             []any{},
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Path, "a") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 warning for oneOf in agent output_schema; got %+v", Validate(ld))
	}
}

func TestSchemaFloorOnlyApppliesToAgents(t *testing.T) {
	// Code steps' output_schemas are NOT subject to the §7 floor — that's an
	// agent-specific cross-backend portability rule. A code step with oneOf is fine.
	ld := makeLD(&Workflow{
		ID: "code-floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{
				ID: "a", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"required":             []any{"x"},
					"properties":           map[string]any{"x": map[string]any{"type": "integer", "minimum": 0}},
					"additionalProperties": false,
				},
			},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" {
			t.Errorf("AWF2002 must not apply to code-step output_schema: %v", d)
		}
	}
}

func TestSchemaFloorMissingRequired(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "missing-req", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"required":             []any{}, // x not required
					"properties":           map[string]any{"x": map[string]any{"type": "integer"}},
					"additionalProperties": false,
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && strings.Contains(d.Message, "x") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for missing-required property; got %+v", Validate(ld))
	}
}

func TestSchemaFloorRecursiveOnNestedObject(t *testing.T) {
	// Anthropic and OpenAI both enforce structural floor rules at every object level.
	// A nested object with additionalProperties: true or missing-required is a real
	// portability problem the validator must surface.
	ld := makeLD(&Workflow{
		ID: "nested-floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"outer"},
					"properties": map[string]any{
						"outer": map[string]any{
							"type":                 "object",
							"additionalProperties": true, // nested violation
							"required":             []any{},
							"properties": map[string]any{
								"x": map[string]any{"type": "string"},
								// "x" missing from required — second nested violation
							},
						},
					},
				},
			},
		},
	})
	warnings := 0
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Path, "a.output_schema") {
			warnings++
		}
	}
	if warnings < 2 {
		t.Errorf("want at least 2 AWF2002 warnings (additionalProperties + required) on nested object; got %d: %+v", warnings, Validate(ld))
	}
}

func TestSchemaInputSchemaAlsoValidated(t *testing.T) {
	bad := JSONSchema{"type": "not-a-type"}
	ld := makeLD(&Workflow{
		ID: "input", Version: 1, Input: &bad,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF2001", "input")
}

func TestSchemaFloorAdditionalPropertiesSchemaForm(t *testing.T) {
	// AWF2002: additionalProperties must be the literal `false`. A SCHEMA OBJECT form
	// (additionalProperties: {type:string}) is valid JSON Schema 2020-12 but forbidden
	// by the §7 floor.
	ld := makeLD(&Workflow{
		ID: "ap-schema", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"}, // schema-form
					"required":             []any{},
					"properties":           map[string]any{},
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Message, "literal `false`") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for additionalProperties schema-form; got %+v", Validate(ld))
	}
}

func TestSchemaFloorTypeAsArrayRejected(t *testing.T) {
	// AWF2002: type as array (e.g. ["string","null"]) is forbidden by the §7 floor.
	// AgentStep with array-typed property surfaces the warning. (The outer schema is
	// type:object so the top-level rule passes; the nested array-type fires.)
	ld := makeLD(&Workflow{
		ID: "type-arr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"x"},
					"properties": map[string]any{
						"x": map[string]any{"type": []any{"string", "null"}}, // array-form type
					},
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Message, "type as an array") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for array-form type; got %+v", Validate(ld))
	}
}
