package ir

import "testing"

// Tests for the tools:/react: cross-reference + adapter-gating pass.
// AWF1052 react tools empty
// AWF1053 react tool name unknown
// AWF1054 react max_turns < 1 (negative; 0 means "use default 8")
// AWF1055 react output_schema declares reserved stop_reason field
// AWF1056 tool impl missing or undeclared container
// AWF1057 react adapter is not awf/llm
// AWF1058 react structured_output: ollama_format

// reactLD builds a minimal LoadedDefinition with one react node and the given tools map.
func reactLD(r *React, tools map[string]Tool) *LoadedDefinition {
	return makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Tools:      tools,
		Graph:      NodeList{r},
	})
}

// okTools returns a minimal valid tools map with one tool backed by the "fin" container.
func okTools() map[string]Tool {
	return map[string]Tool{"check": {
		Description: "d",
		InputSchema: &JSONSchema{"type": "object"},
		Impl:        ToolImpl{Run: "true", Container: "fin"},
	}}
}

// --- Task 1.4 tests ---

func TestValidateReactToolsEmpty(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: nil, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1052", "react[0]")
}

func TestValidateReactToolUnknown(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"missing"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1053", "react[0]")
}

func TestValidateReactMaxTurnsNonPositive(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, MaxTurns: -1, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1054", "react[0]")
}

func TestValidateReactOutputSchemaReservesStopReason(t *testing.T) {
	r := &React{
		ID: "a", Prompt: "q", Tools: []string{"check"},
		With: RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &JSONSchema{
			"type":       "object",
			"properties": map[string]any{"stop_reason": map[string]any{"type": "string"}},
		},
	}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1055", "react[0]")
}

func TestValidateToolImplMissingContainer(t *testing.T) {
	tools := map[string]Tool{
		"check": {
			Description: "d",
			InputSchema: &JSONSchema{"type": "object"},
			Impl:        ToolImpl{Run: "true"}, // no Container
		},
	}
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, tools)), "AWF1056", "tools.check")
}

func TestValidateToolImplUndeclaredContainer(t *testing.T) {
	tools := map[string]Tool{
		"check": {
			Description: "d",
			InputSchema: &JSONSchema{"type": "object"},
			Impl:        ToolImpl{Run: "true", Container: "nonexistent"},
		},
	}
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, tools)), "AWF1056", "tools.check")
}

// MaxTurns == 0 means "use default 8" — must NOT trigger AWF1054.
func TestValidateReactMaxTurnsZeroIsDefault(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, MaxTurns: 0, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertNoErrorCode(t, Validate(reactLD(r, okTools())), "AWF1054")
}

// --- Task 1.5 tests ---

func TestValidateReactRejectsNonAwfllm(t *testing.T) {
	r := &React{
		ID: "a", Prompt: "q", Tools: []string{"check"},
		With: RawConfig{"uses": "anthropic/claude-code", "model": "m"},
	}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1057", "react[0]")
}

func TestValidateReactRejectsOllamaFormat(t *testing.T) {
	r := &React{
		ID: "a", Prompt: "q", Tools: []string{"check"},
		With: RawConfig{"uses": "awf/llm", "model": "m", "structured_output": "ollama_format"},
	}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1058", "react[0]")
}
