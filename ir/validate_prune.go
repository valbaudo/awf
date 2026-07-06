package ir

import "strings"

// validatePrune checks every map `prune:` clause (SP5, spec §3.2b). prune is a
// map-only clause — parallel has no Prune field and no wire surface (Task 2/5),
// so this pass only switches on *Map.
//   - shape (AWF1037): exactly one of keep/stop_when; non-empty score; keep is a
//     positive integer; non-empty stop_when.
//   - score binding (AWF5008): score must name a numeric field in the body's
//     last step's output_schema (the engine reads it as a typed number).
//
// stop_when is NOT statically type-checked here — it fails at runtime via
// template.EvalBoolString, mirroring loop.until.
func validatePrune(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	WalkNodes(ld.Workflow.Graph, "", func(n Node, nodePath string) {
		m, ok := n.(*Map)
		if !ok || m.Prune == nil {
			return
		}
		pr := m.Prune
		// Shape (AWF1037).
		hasKeep := pr.Keep != nil
		hasStop := strings.TrimSpace(pr.StopWhen) != ""
		if pr.Score == "" || hasKeep == hasStop /* both or neither */ {
			c.errf(nodePath, "AWF1037", catalog["AWF1037"])
			return
		}
		if hasKeep && pr.Keep.K <= 0 {
			c.errf(nodePath, "AWF1037", catalog["AWF1037"])
			return
		}
		// Score binding (AWF5008): the body's last step must declare `score` as a
		// numeric field in its output_schema.
		last := lastStepSchema(m.Body)
		if last == nil || !schemaHasNumericField(last, pr.Score) {
			c.errf(nodePath, "AWF5008", catalog["AWF5008"])
		}
	})
}

// lastStepSchema returns the output_schema of the body's LAST node when that node
// is a code/agent step, or nil otherwise (empty body, or a control-flow terminal
// node whose output is not a single step's schema — rejected as AWF5008).
func lastStepSchema(body NodeList) *JSONSchema {
	if len(body) == 0 {
		return nil
	}
	switch s := body[len(body)-1].(type) {
	case *CodeStep:
		return s.OutputSchema
	case *AgentStep:
		return s.OutputSchema
	default:
		return nil
	}
}

// schemaHasNumericField reports whether the JSON Schema declares `field` as a
// number/integer property. JSONSchema is map[string]any (ir/types.go;
// confirmed: ir/validate_refs.go:270 does (*schema)["properties"].(map[string]any)),
// so introspection is map indexing — no hand-rolled JSON Schema parser. This
// mirrors checkSchemaField (ir/validate_refs.go:265-276), which checks field
// EXISTENCE; here we additionally read the declared `type`.
func schemaHasNumericField(schema *JSONSchema, field string) bool {
	if schema == nil {
		return false
	}
	props, _ := (*schema)["properties"].(map[string]any)
	spec, ok := props[field].(map[string]any)
	if !ok {
		return false // field absent OR not an object schema
	}
	t, _ := spec["type"].(string)
	return t == "number" || t == "integer"
}
