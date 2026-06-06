package ir

import "testing"

// Tests for the AWF1026-AWF1031 continues: pass — see validate_continues.go.
// NOTE: AWF1025 is reserved for the map-image-target-static-pin rule; continues:
// validation starts at AWF1026 (next free code after the existing AWF1025).

func TestContinuesTargetMustExist(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "critique", Uses: "awf/llm", Continues: "nope", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1026", "critique")
}

func TestContinuesTargetMustBeAgentStep(t *testing.T) {
	// A continues: pointing at a CodeStep (or any non-agent node) is AWF1026.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "build", Container: "c", Run: "true"},
			&AgentStep{ID: "critique", Uses: "awf/llm", Continues: "build", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1026", "critique")
}

func TestContinuesEmptyIsClean(t *testing.T) {
	// No continues: → no AWF1026-1031 of any kind.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	for _, code := range []string{"AWF1026", "AWF1027", "AWF1028", "AWF1029", "AWF1030", "AWF1031"} {
		assertNoCode(t, Validate(ld), code)
	}
}
