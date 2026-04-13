package engine

import (
	"encoding/json"
	"errors"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/valbaudo/awf/ir"
)

// ValidateAgainstSchema decodes raw as JSON and validates the decoded value
// against schema using santhosh-tekuri/jsonschema/v6 — the same library
// slice 1.4 uses for static schema-well-formedness checks. Returns the
// decoded map on success; errors wrap the decode / compile / validation
// failure cause.
//
// Used by:
//   - engine/local_dispatcher.go's runCode (validates $AWF_OUTPUT against
//     the step's output_schema).
//   - cli/run.go's cliRun (validates --input JSON against workflow.input).
//
// The schema cast (map[string]any(*schema)) works because IR schemas come
// from JSON-unmarshaled YAML — all nested values are native map[string]any,
// not the JSONSchema defined-type. Slice 2.4 Revision #3 documented the
// defensive pattern for Go-constructed schemas: round-trip via
// json.Marshal/Unmarshal first. The Phase 2 fixture set + CLI input path
// stays on the YAML-decoded path so this isn't a concern; the docstring
// preserves the warning for future callers.
func ValidateAgainstSchema(raw []byte, schema *ir.JSONSchema) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, errors.New("ValidateAgainstSchema: empty input")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("ValidateAgainstSchema: decode: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("engine://schema", map[string]any(*schema)); err != nil {
		return nil, fmt.Errorf("ValidateAgainstSchema: schema compile: %w", err)
	}
	compiled, err := c.Compile("engine://schema")
	if err != nil {
		return nil, fmt.Errorf("ValidateAgainstSchema: schema compile: %w", err)
	}
	if err := compiled.Validate(decoded); err != nil {
		return nil, fmt.Errorf("ValidateAgainstSchema: schema validation: %w", err)
	}
	return decoded, nil
}
