package loader

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestDesugarJuryProducesMapQuorum(t *testing.T) {
	q := ir.Ratio("2")
	step := &ir.AgentStep{
		ID: "judge", Uses: "openai/codex", Container: "judge",
		With: ir.RawConfig{"prompt": "review", "model": "base"},
		OutputSchema: &ir.JSONSchema{"type": "object",
			"properties": map[string]any{
				"accept":   map[string]any{"type": "boolean"},
				"critique": map[string]any{"type": "string"},
			}},
		Jury: &ir.Jury{
			Over:   []map[string]any{{"model": "gpt-5"}, {"model": "o3"}, {"model": "sonnet-5"}},
			Quorum: &q, Field: "accept",
		},
	}
	wf := &ir.Workflow{Graph: ir.NodeList{step}}
	desugarJury(wf)

	m, ok := wf.Graph[0].(*ir.Map)
	if !ok {
		t.Fatalf("Graph[0] = %T, want *ir.Map", wf.Graph[0])
	}
	if m.ID != "" {
		t.Errorf("map id = %q, want empty (jury map is anonymous; verdict is positional via evaluate.<field>, and a named map under a gate trips AWF5011)", m.ID)
	}
	if len(m.OverItems) != 3 {
		t.Errorf("over items = %d, want 3", len(m.OverItems))
	}
	if m.As == "" {
		t.Fatalf("map.As empty; a binding name is required for {{ <as>.model }}")
	}
	if m.Reduce == nil || !m.Reduce.IsQuorum() || m.Reduce.Field != "accept" {
		t.Fatalf("reduce = %+v, want quorum field=accept", m.Reduce)
	}
	body, ok := m.Body[0].(*ir.AgentStep)
	if !ok {
		t.Fatalf("body[0] = %T, want *ir.AgentStep", m.Body[0])
	}
	if body.Jury != nil {
		t.Errorf("desugared body step still carries Jury")
	}
	// The varied key `model` is templated from the binding, as a plain string —
	// matching how a decoded with: string value is represented (see
	// engine/agent_step.go's substituteRawConfig, which type-asserts a RawConfig
	// value as `string`, never ir.Template). The untouched key `prompt` passes
	// through verbatim.
	if got := body.With["model"]; got != "{{ "+m.As+".model }}" {
		t.Errorf("body.with[model] = %v, want templated {{ %s.model }}", got, m.As)
	}
	if body.With["prompt"] != "review" {
		t.Errorf("body.with[prompt] = %v, want passthrough 'review'", body.With["prompt"])
	}
}

// TestJurySugarDesugaredByteIdentical proves the E1 promise: a jury: block and
// its hand-written map+quorum equivalent normalize to the same digest once
// loaded. testdata/valid/jury-sugar.yaml and jury-desugared.yaml differ ONLY in
// which of the two surface forms they use — both place the panel as the last
// node of a gate.evaluate (the realistic, forward-compatible shape Task 4's
// AWF1072 will require).
func TestJurySugarDesugaredByteIdentical(t *testing.T) {
	sugar, err := Load("testdata/valid/jury-sugar.yaml")
	if err != nil {
		t.Fatalf("load jury-sugar.yaml: %v", err)
	}
	handWritten, err := Load("testdata/valid/jury-desugared.yaml")
	if err != nil {
		t.Fatalf("load jury-desugared.yaml: %v", err)
	}
	dSugar, err := sugar.ComputeDigest()
	if err != nil {
		t.Fatalf("digest jury-sugar: %v", err)
	}
	dHandWritten, err := handWritten.ComputeDigest()
	if err != nil {
		t.Fatalf("digest jury-desugared: %v", err)
	}
	if dSugar != dHandWritten {
		t.Fatalf("digest mismatch: jury: sugar = %s, hand-written map+quorum = %s", dSugar, dHandWritten)
	}
}
