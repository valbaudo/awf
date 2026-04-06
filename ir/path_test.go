package ir

import "testing"

// TestPathFor covers the static IR path scheme used by validation diagnostics.
// Step nodes are addressed by their id; control nodes positionally with a keyword[index] form.
// The runtime path function (engine/path, Phase 2) extends this with iter/attempt suffixes;
// the static prefix matches.
func TestPathFor(t *testing.T) {
	cases := []struct {
		name    string
		parent  string
		keyword string // empty for step nodes
		stepID  string // empty for control nodes
		index   int    // sibling index in parent's children
		want    string
	}{
		{"top-level step by id", "", "", "triage", 0, "triage"},
		{"top-level if", "", "if", "", 1, "if[1]"},
		{"top-level try", "", "try", "", 2, "try[2]"},
		{"step inside if.then", "if[1].then", "", "approve", 0, "if[1].then.approve"},
		{"nested if inside loop body", "loop[0].body", "if", "", 3, "loop[0].body.if[3]"},
		{"gate generate child", "gate[0].generate", "", "exploit", 0, "gate[0].generate.exploit"},
		{"parallel child by id", "parallel[0]", "", "branch_a", 0, "parallel[0].branch_a"},
		{"both set: stepID wins (defensive tie-break)", "", "if", "approve", 0, "approve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PathFor(c.parent, c.keyword, c.stepID, c.index)
			if got != c.want {
				t.Errorf("PathFor(%q, %q, %q, %d) = %q, want %q",
					c.parent, c.keyword, c.stepID, c.index, got, c.want)
			}
		})
	}
}

// Containers metadata uses a dotted path; this is a small helper that just joins.
func TestContainerPath(t *testing.T) {
	if got := ContainerPath("lab", "image"); got != "containers.lab.image" {
		t.Errorf("ContainerPath = %q", got)
	}
	if got := ContainerPath("lab", ""); got != "containers.lab" {
		t.Errorf("ContainerPath no-field = %q", got)
	}
}

// TestChildPath pins the convention every validation pass uses to address a control node's
// named child block. Centralized so the four walkers (structural / refs / schema / index)
// can't drift apart on branch-name spellings.
func TestChildPath(t *testing.T) {
	cases := []struct {
		parent, keyword, branch string
		idx                     int
		want                    string
	}{
		{"", "if", "then", 1, "if[1].then"},
		{"", "if", "else", 1, "if[1].else"},
		{"", "loop", "body", 0, "loop[0].body"},
		{"", "try", "do", 0, "try[0].do"},
		{"", "try", "catch", 0, "try[0].catch"},
		{"", "try", "finally", 0, "try[0].finally"},
		{"", "gate", "generate", 2, "gate[2].generate"},
		{"", "gate", "evaluate", 2, "gate[2].evaluate"},
		{"", "map", "body", 0, "map[0].body"},
		{"loop[0].body", "if", "then", 3, "loop[0].body.if[3].then"},
	}
	for _, c := range cases {
		got := ChildPath(c.parent, c.keyword, c.idx, c.branch)
		if got != c.want {
			t.Errorf("ChildPath(%q, %q, %d, %q) = %q, want %q",
				c.parent, c.keyword, c.idx, c.branch, got, c.want)
		}
	}
}
