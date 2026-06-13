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

// --- P3 review fix: tool-impl input_files validation ---
// A tool impl's input_files refs get the SAME static checks a CodeStep's do
// (AWF3007), validated relative to EACH react node that offers the tool.

// stagingTool returns a tool whose impl stages one input_files ref.
func stagingTool(dst, ref string) map[string]Tool {
	return map[string]Tool{"check": {
		Description: "d",
		InputSchema: &JSONSchema{"type": "object"},
		Impl:        ToolImpl{Run: "true", Container: "fin", InputFiles: map[string]string{dst: ref}},
	}}
}

// An asset.<id> ref naming a declared workflow asset is accepted.
func TestValidateToolImplInputFilesAssetRefAccepted(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Assets:     map[string]string{"fixture": "fixtures/f.json"},
		Tools:      stagingTool("/work/f.json", "asset.fixture"),
		Graph:      NodeList{r},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

// An undeclared asset ref → AWF3007 at the react node that offers the tool.
func TestValidateToolImplInputFilesUnknownAssetReportsAWF3007(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Assets:     map[string]string{"fixture": "fixtures/f.json"},
		Tools:      stagingTool("/work/f.json", "asset.missing"),
		Graph:      NodeList{r},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "react[0]")
}

// A malformed ref (not step.<id>.files.<name> / input.files.<name> / asset.<id>)
// → AWF3007 at the react node.
func TestValidateToolImplInputFilesMalformedRefReportsAWF3007(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Tools:      stagingTool("/work/f.json", "step.recon.report"), // missing .files.
		Graph:      NodeList{r},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "react[0]")
}

// A step.<id>.files.<name> ref to a producer that runs BEFORE the react node is
// accepted (producer-order satisfied relative to the consuming react node).
func TestValidateToolImplInputFilesStepRefBeforeReactAccepted(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}, "c": {Image: "oci://y@sha256:def"}},
		Tools:      stagingTool("/work/report.md", "step.recon.files.report"),
		Graph: NodeList{
			&CodeStep{ID: "recon", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}}},
			r,
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

// A step.<id>.files.<name> ref to a producer that runs AFTER the react node is a
// forward reference → AWF3007 at the react node (producer-order violated).
func TestValidateToolImplInputFilesForwardStepRefReportsAWF3007(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}, "c": {Image: "oci://y@sha256:def"}},
		Tools:      stagingTool("/work/report.md", "step.recon.files.report"),
		Graph: NodeList{
			r, // react node BEFORE the producer
			&CodeStep{ID: "recon", Container: "c", Run: "true",
				OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "react[0]")
}

// A non-absolute destination path → AWF3007 at the react node.
func TestValidateToolImplInputFilesBadDestReportsAWF3007(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	ld := makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Assets:     map[string]string{"fixture": "fixtures/f.json"},
		Tools:      stagingTool("work/f.json", "asset.fixture"), // relative dst
		Graph:      NodeList{r},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "react[0]")
}
