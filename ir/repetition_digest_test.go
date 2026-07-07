package ir_test

import (
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// F38: YAML anchors (&name/*name), the <<: merge key, and a reserved top-level
// x-* holder are the documented way to factor out repeated blocks (see man
// REDUCING REPETITION). These are resolved by goccy at YAML-parse time —
// frontend/yaml.DecodeWithRaw's stage 1 (goyaml.Unmarshal into `any`) — strictly
// BEFORE the raw tree is JSON-round-tripped into the IR (stage 3), so the IR the
// engine digests never sees an anchor, an alias, or a merge key: it only ever sees
// the already-expanded scalar/map values. This pair of tests proves that
// end-to-end for the exact repetition shape from the task brief (51x
// `container: workspace` collapsed to one x-defaults anchor):
//
//   - testdata/repetition_anchors.yaml uses &defaults/<<: plus an x-defaults holder.
//   - testdata/repetition_expanded.yaml is the byte-for-byte hand-expanded twin:
//     same three steps, container:/uses: spelled out on each, no x-defaults key.
//
// (a) below: the anchored file validates clean — the x-defaults holder is not
// flagged AWF1062, and the merged steps are structurally valid.
// (b) below: LoadedDefinition.ComputeDigest() is IDENTICAL between the two files —
// proving the anchor/merge expansion is purely a source-level convenience with zero
// effect on the content-addressed definition digest that resume pins against.
func TestReducingRepetitionAnchors_ValidatesClean(t *testing.T) {
	ld, err := loader.Load("testdata/repetition_anchors.yaml")
	if err != nil {
		t.Fatalf("loader.Load(repetition_anchors.yaml): %v", err)
	}
	diags := ir.Validate(ld)
	for _, d := range diags {
		if d.Severity == ir.Error {
			t.Errorf("unexpected Error diagnostic %s at %s: %s", d.Code, d.Path, d.Message)
		}
		if d.Code == "AWF1062" {
			t.Errorf("x-defaults holder must not be flagged AWF1062, got %v", d)
		}
	}
	if ir.HasErrors(diags) {
		t.Fatalf("anchored/merged workflow failed to validate clean: %v", diags)
	}

	// Sanity: the anchor/merge actually materialized three fully-formed steps
	// (not, say, three empty stubs that happen to validate for lack of content).
	if got := len(ld.Workflow.Graph); got != 3 {
		t.Fatalf("expected 3 graph nodes after merge expansion, got %d", got)
	}
	for _, n := range ld.Workflow.Graph {
		step, ok := n.(*ir.AgentStep)
		if !ok {
			t.Fatalf("expected *ir.AgentStep, got %T", n)
		}
		if step.Container != "workspace" {
			t.Errorf("step %s: <<: *defaults did not merge container:, got %q", step.ID, step.Container)
		}
		if step.Uses != "anthropic/claude-code" {
			t.Errorf("step %s: <<: *defaults did not merge uses:, got %q", step.ID, step.Uses)
		}
	}

	// The x-* holder itself is visible in the pre-IR raw tree (proving the anchor
	// really did resolve there) but corresponds to no Workflow field, so it never
	// reaches the typed IR — see (b) below, where its presence/absence is digest-inert.
	root := ld.Modules[""]
	if root == nil || root.RawDoc == nil {
		t.Fatal("expected root module RawDoc to be populated")
	}
	if _, ok := root.RawDoc["x-defaults"]; !ok {
		t.Fatal("expected x-defaults to appear in the raw pre-IR tree")
	}
}

func TestReducingRepetitionAnchors_DigestMatchesHandExpanded(t *testing.T) {
	anchored, err := loader.Load("testdata/repetition_anchors.yaml")
	if err != nil {
		t.Fatalf("loader.Load(repetition_anchors.yaml): %v", err)
	}
	expanded, err := loader.Load("testdata/repetition_expanded.yaml")
	if err != nil {
		t.Fatalf("loader.Load(repetition_expanded.yaml): %v", err)
	}

	dAnchored, err := anchored.ComputeDigest()
	if err != nil {
		t.Fatalf("anchored.ComputeDigest(): %v", err)
	}
	dExpanded, err := expanded.ComputeDigest()
	if err != nil {
		t.Fatalf("expanded.ComputeDigest(): %v", err)
	}

	if dAnchored != dExpanded {
		t.Fatalf("anchor/merge expansion changed the definition digest:\n anchored = %s\n expanded = %s\n(anchors are resolved before the IR — these must be byte-identical)", dAnchored, dExpanded)
	}
}
