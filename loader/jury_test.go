package loader

import (
	"errors"
	"path/filepath"
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

// TestDesugarJuryDefaultsOmittedField proves Task 4's binding item: an omitted
// field: (the Jury struct's documented default) resolves to the sole boolean
// output_schema property via ir.JuryField, instead of desugaring to
// Reduce{Field:""} (which would surface a misleading AWF1035 on the emitted
// map rather than reflecting the jury: block's own default).
func TestDesugarJuryDefaultsOmittedField(t *testing.T) {
	q := ir.Ratio("2")
	step := &ir.AgentStep{
		ID: "judge", Uses: "openai/codex", Container: "judge",
		With: ir.RawConfig{"model": "base"},
		OutputSchema: &ir.JSONSchema{"type": "object",
			"properties": map[string]any{
				"accept":   map[string]any{"type": "boolean"},
				"critique": map[string]any{"type": "string"},
			}},
		Jury: &ir.Jury{
			Over:   []map[string]any{{"model": "gpt-5"}, {"model": "o3"}},
			Quorum: &q, // Field omitted
		},
	}
	wf := &ir.Workflow{Graph: ir.NodeList{step}}
	desugarJury(wf)

	m, ok := wf.Graph[0].(*ir.Map)
	if !ok {
		t.Fatalf("Graph[0] = %T, want *ir.Map", wf.Graph[0])
	}
	if m.Reduce == nil || m.Reduce.Field != "accept" {
		t.Fatalf("reduce.field = %+v, want defaulted to the sole boolean property %q", m.Reduce, "accept")
	}
}

// TestLoadJuryValidationIsDeterministicAcrossModules proves the fix for the
// nondeterministic-module-selection bug: modules is a map, so a naive `for
// range modules { if errs := ...; return juryLoadError(errs) }` would surface
// WHICHEVER module's malformed jury: happened to be visited first in Go's
// randomized map iteration order. Both the root workflow and its import here
// carry a malformed jury: (missing output_schema, AWF1069); Load must collect
// diagnostics from every module before choosing (sorted by path, then code —
// see juryLoadError), so the same one — the alphabetically-first path,
// "aaa_child_judge" in the imported module — surfaces on every call.
func TestLoadJuryValidationIsDeterministicAcrossModules(t *testing.T) {
	dir := t.TempDir()
	childBody := "workflow: child\nversion: 1\ncontainers:\n  c:\n    image: oci://example.com/x@sha256:2222222222222222222222222222222222222222222222222222222222222222\ngraph:\n  - gate:\n      generate:\n        - id: gen\n          container: c\n          run: ./propose.sh\n      evaluate:\n        - id: aaa_child_judge\n          container: c\n          uses: openai/codex\n          with:\n            model: base\n          jury:\n            over:\n              - model: a\n              - model: b\n            quorum: 2\n      until: \"{{ evaluate.accept }}\"\n      max_attempts: 3\n"
	writeFile(t, filepath.Join(dir, "child.awf.yaml"), childBody)
	rootBody := "workflow: root\nversion: 1\nimports:\n  child: child.awf.yaml\ncontainers:\n  c:\n    image: oci://example.com/x@sha256:1111111111111111111111111111111111111111111111111111111111111111\ngraph:\n  - gate:\n      generate:\n        - id: gen\n          container: c\n          run: ./propose.sh\n      evaluate:\n        - id: zzz_root_judge\n          container: c\n          uses: openai/codex\n          with:\n            model: base\n          jury:\n            over:\n              - model: a\n              - model: b\n            quorum: 2\n      until: \"{{ evaluate.accept }}\"\n      max_attempts: 3\n"
	rootPath := writeWorkflow(t, dir, rootBody)

	const wantPath = "gate[0].evaluate.aaa_child_judge"
	for i := 0; i < 10; i++ {
		_, err := Load(rootPath)
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("iteration %d: err = %T %v, want *LoadError", i, err, err)
		}
		if le.Code != "AWF1069" || le.Path != wantPath {
			t.Fatalf("iteration %d: LoadError = %+v, want code AWF1069 at %s (root AND child both violate; selection must not depend on map iteration order)", i, le, wantPath)
		}
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
