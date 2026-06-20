package ir

// Tests for the AWF1061 misplaced-with:-key warning — see validate_misplaced_with.go.

import "testing"

// TestMisplacedWithKeyInputFilesWarnsAWF1061 checks that an agent step with
// "input_files" nested inside its with: block produces an AWF1061 Warning.
func TestMisplacedWithKeyInputFilesWarnsAWF1061(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "mwk", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code",
				With: RawConfig{
					"prompt":      "do the thing",
					"input_files": map[string]any{"/dst": "step.prev.files.out"},
				},
			},
		},
	})
	assertWarningAt(t, Validate(ld), "AWF1061", "a")
}

// TestMisplacedWithKeyOutputSchemaWarnsAWF1061 checks that an agent step with
// "output_schema" nested inside its with: block also produces an AWF1061 Warning.
func TestMisplacedWithKeyOutputSchemaWarnsAWF1061(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "mwk2", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "b", Container: "c", Uses: "anthropic/claude-code",
				With: RawConfig{
					"model":         "claude-opus-4-5",
					"output_schema": map[string]any{"type": "object"},
				},
			},
		},
	})
	assertWarningAt(t, Validate(ld), "AWF1061", "b")
}

// TestMisplacedWithKeyLegitKeysNoAWF1061 asserts that legitimate with: keys
// (prompt, model) do NOT trigger AWF1061.
func TestMisplacedWithKeyLegitKeysNoAWF1061(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "mwk3", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "c", Container: "c", Uses: "anthropic/claude-code",
				With: RawConfig{
					"prompt": "do the thing",
					"model":  "claude-haiku-4-5",
				},
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1061")
}
