// Package yaml is the YAML frontend: parses YAML workflows into the ir.Workflow IR. The decode path
// is YAML → any (goccy) → JSON (stdlib) → IR (the existing JSON unmarshalers in ir/), reusing the
// key-presence Node dispatch, the standard-surface Skip and Parallel marshalers, Duration's
// string/integer forms, and the digest-stable map-key ordering — without duplicating any of that.
//
// Parse-time errors from goccy carry line:column inside *goccy/go-yaml/errors.SyntaxError
// (reachable via errors.As), so callers can surface position-aware diagnostics for YAML syntax
// issues. Position-aware SEMANTIC diagnostics (e.g. "field X at line Y is invalid") are deferred
// — a re-parse of the YAML AST by future validation would recover them when needed.
package yaml

import (
	"encoding/json"
	"fmt"

	goyaml "github.com/goccy/go-yaml"

	"github.com/valbaudo/awf/ir"
)

// Decode parses a YAML workflow document into an *ir.Workflow. The returned error wraps goccy's
// parse error (with line:column) on syntax failures; downstream errors include the failing stage
// for attribution.
func Decode(yamlBytes []byte) (*ir.Workflow, error) {
	wf, _, err := DecodeWithRaw(yamlBytes)
	return wf, err
}

// DecodeWithRaw parses a YAML workflow document into an *ir.Workflow, same as Decode, and also
// returns the top-level raw mapping from stage 1 of the decode pipeline (nil if the document's
// top level isn't a mapping). The raw tree lets a validator detect keys/values the typed
// unmarshal silently discarded — Decode itself doesn't need it, so it stays a thin wrapper.
func DecodeWithRaw(yamlBytes []byte) (*ir.Workflow, map[string]any, error) {
	// Stage 1: YAML → any. goccy unmarshals YAML mappings into map[string]any, which is exactly
	// what encoding/json round-trips through.
	var raw any
	if err := goyaml.Unmarshal(yamlBytes, &raw); err != nil {
		return nil, nil, fmt.Errorf("yaml parse: %w", err)
	}
	// Stage 2: any → JSON. json.Marshal of map[string]any sorts keys, so the intermediate
	// JSON is deterministic for diagnostics.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("yaml→json: %w", err)
	}
	// Stage 3: JSON → IR. Reuses every existing unmarshaler in ir/ (Node key-presence, Skip
	// string-form, Parallel array-form, Duration int/string, JSONSchema map, etc.).
	var wf ir.Workflow
	if err := json.Unmarshal(jsonBytes, &wf); err != nil {
		return nil, nil, fmt.Errorf("json→ir: %w", err)
	}
	rawMap, _ := raw.(map[string]any)
	return &wf, rawMap, nil
}
