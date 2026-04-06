package ir

import (
	"encoding/json"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// validateSchema runs the AWF2001 (well-formedness) and AWF2002 (§7 floor, agents only) pass.
//
//   - AWF2001 — every JSON Schema in the IR (the workflow's `input`, every step's
//     `output_schema`) must compile against the JSON Schema 2020-12 metaschema. We use
//     santhosh-tekuri/jsonschema/v6: NewCompiler().AddResource(url, decoded) → Compile(url).
//
//   - AWF2002 — agent output_schemas must additionally satisfy the §7 conservative
//     cross-backend floor: `type: object`, `additionalProperties: false`, every property in
//     `required`, allowed types (object/array/string/integer/number/boolean + enum), no
//     `oneOf`/`anyOf`/`allOf`/`not`, no range/format keywords (minimum/maximum/minLength/
//     pattern/format/…), nesting depth ≤ 10. The floor is a hand-rolled walk over the raw
//     decoded map — santhosh-tekuri's compiled-schema API doesn't expose enough surface to
//     check keyword PRESENCE; raw-map traversal is the right tool.
//
// Code-step output_schemas are NOT subject to the floor (that's an agent-cross-backend
// portability rule; code steps produce JSON via $AWF_OUTPUT and the runtime decodes against
// the schema directly, no backend constraint involved).
func validateSchema(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow
	if wf.Input != nil {
		checkSchemaWellFormed(*wf.Input, "input", c)
	}
	walkSchemas(wf.Graph, "", c)
}

func walkSchemas(nodes NodeList, parent string, c *collector) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			if v.OutputSchema != nil {
				path := PathFor(parent, "", v.ID, i) + ".output_schema"
				checkSchemaWellFormed(*v.OutputSchema, path, c)
			}
		case *AgentStep:
			if v.OutputSchema != nil {
				path := PathFor(parent, "", v.ID, i) + ".output_schema"
				checkSchemaWellFormed(*v.OutputSchema, path, c)
				checkAgentFloor(*v.OutputSchema, path, c)
			}
		case *SignalStep:
			if v.OutputSchema != nil {
				path := PathFor(parent, "", v.ID, i) + ".output_schema"
				checkSchemaWellFormed(*v.OutputSchema, path, c)
			}
		case *If:
			walkSchemas(v.Then, ChildPath(parent, "if", i, "then"), c)
			walkSchemas(v.Else, ChildPath(parent, "if", i, "else"), c)
		case *Loop:
			walkSchemas(v.Body, ChildPath(parent, "loop", i, "body"), c)
		case *Try:
			walkSchemas(v.Do, ChildPath(parent, "try", i, "do"), c)
			walkSchemas(v.Catch, ChildPath(parent, "try", i, "catch"), c)
			walkSchemas(v.Finally, ChildPath(parent, "try", i, "finally"), c)
		case *Parallel:
			walkSchemas(v.Children, PathFor(parent, "parallel", "", i), c)
		case *Gate:
			walkSchemas(v.Generate, ChildPath(parent, "gate", i, "generate"), c)
			walkSchemas(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c)
		case *Map:
			walkSchemas(v.Body, ChildPath(parent, "map", i, "body"), c)
		}
	}
}

func checkSchemaWellFormed(schema JSONSchema, path string, c *collector) {
	// Round-trip through JSON to convert the JSONSchema (map[string]any) into the form
	// the compiler expects.
	raw, err := json.Marshal(schema)
	if err != nil {
		c.errf(path, "AWF2001", fmt.Sprintf("re-marshal: %s", err))
		return
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		c.errf(path, "AWF2001", fmt.Sprintf("re-unmarshal: %s", err))
		return
	}
	cm := jsonschema.NewCompiler()
	const url = "inline://schema"
	if err := cm.AddResource(url, decoded); err != nil {
		c.errf(path, "AWF2001", err.Error())
		return
	}
	if _, err := cm.Compile(url); err != nil {
		c.errf(path, "AWF2001", err.Error())
	}
}

// Forbidden composition keywords (the §7 floor REJECTS these in agent schemas).
var floorForbiddenComposition = []string{"oneOf", "anyOf", "allOf", "not"}

// Forbidden range/format keywords.
var floorForbiddenRange = []string{
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
	"minLength", "maxLength", "minItems", "maxItems", "pattern", "format",
}

// Allowed JSON Schema types per the §7 floor.
var floorAllowedTypes = map[string]bool{
	"object": true, "array": true, "string": true,
	"integer": true, "number": true, "boolean": true,
}

// floorMaxDepth is the §7 nesting cap.
const floorMaxDepth = 10

// checkAgentFloor enforces the §7 conservative cross-backend floor on an agent's
// output_schema. The floor's structural requirements (type: object, additionalProperties:
// false, all properties in required) must hold at EVERY object-typed depth — Anthropic and
// OpenAI both enforce these recursively in their constrained-output modes, so a top-level-
// only check would let through schemas that the backends will reject at run time.
//
// All checks (structural + forbidden keywords + type whitelist + depth cap) live inside
// walkFloor so they recurse uniformly.
func checkAgentFloor(schema JSONSchema, path string, c *collector) {
	walkFloor(map[string]any(schema), path, c, 0)
}

func walkFloor(m map[string]any, path string, c *collector, depth int) {
	if depth > floorMaxDepth {
		c.warnf(path, "AWF2002", fmt.Sprintf("nesting depth exceeds %d", floorMaxDepth))
		return
	}
	// Forbidden composition + range/format keywords at every depth.
	for _, k := range floorForbiddenComposition {
		if _, has := m[k]; has {
			c.warnf(path, "AWF2002", fmt.Sprintf("%s not allowed (forbidden by §7 floor)", k))
		}
	}
	for _, k := range floorForbiddenRange {
		if _, has := m[k]; has {
			c.warnf(path, "AWF2002", fmt.Sprintf("%s not allowed (forbidden by §7 floor)", k))
		}
	}
	// Type restriction (skipped if `type` absent; absence is caught elsewhere if it matters).
	// Fix 2: type as array (e.g. ["string","null"]) is forbidden by §7 floor.
	switch t := m["type"].(type) {
	case string:
		if t != "" {
			if _, allowed := floorAllowedTypes[t]; !allowed && !hasEnum(m) {
				c.warnf(path, "AWF2002", fmt.Sprintf("type %q not allowed by §7 floor", t))
			}
		}
	case []any:
		// Array-form type (e.g. ["string","null"]) is not allowed by the §7 floor — neither
		// Anthropic nor OpenAI structured-output modes accept array-typed `type`.
		c.warnf(path, "AWF2002", "type as an array is not allowed by §7 floor (use a single scalar type)")
	}
	// Object-typed schemas — at EVERY depth, not just top-level — must satisfy the structural
	// trio (type: object, additionalProperties: false, all props in required). Anthropic and
	// OpenAI both enforce this recursively in constrained-output modes.
	if t, _ := m["type"].(string); t == "object" {
		if ap, ok := m["additionalProperties"]; ok {
			// Fix 1: additionalProperties must be the literal `false`. A schema-object form
			// (additionalProperties: {type:string}) is valid JSON Schema but forbidden by §7 floor.
			if b, isBool := ap.(bool); !isBool || b {
				c.warnf(path, "AWF2002", "additionalProperties must be the literal `false` (at every object level)")
			}
		} else {
			c.warnf(path, "AWF2002", "additionalProperties must be explicitly false")
		}
		props, _ := m["properties"].(map[string]any)
		req, _ := m["required"].([]any)
		reqSet := map[string]bool{}
		for _, r := range req {
			if s, ok := r.(string); ok {
				reqSet[s] = true
			}
		}
		for name := range props {
			if !reqSet[name] {
				c.warnf(path, "AWF2002", fmt.Sprintf("property %q must be in required (at every object level)", name))
			}
		}
	}
	// At the top level only: also enforce that the schema itself is an object (the floor
	// rejects non-object root schemas for agent outputs because typed-output decoding
	// expects a JSON object).
	if depth == 0 {
		if t, _ := m["type"].(string); t != "object" {
			c.warnf(path, "AWF2002", fmt.Sprintf("top-level type must be \"object\", got %q", t))
		}
	}
	// Recurse into properties + items.
	if props, ok := m["properties"].(map[string]any); ok {
		for name, sub := range props {
			if subMap, ok := sub.(map[string]any); ok {
				walkFloor(subMap, path+".properties."+name, c, depth+1)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		walkFloor(items, path+".items", c, depth+1)
	}
}

// hasEnum reports whether the schema declares an `enum` keyword at this level. `enum` is
// always allowed by the §7 floor regardless of the surrounding `type`, so its presence
// shortcuts the type-allowlist check above.
func hasEnum(m map[string]any) bool {
	_, ok := m["enum"]
	return ok
}
