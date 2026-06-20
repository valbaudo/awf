package engine

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestIsDeterministicNode(t *testing.T) {
	tests := []struct {
		name string
		node ir.Node
		want bool
	}{
		{"CodeStep true", &ir.CodeStep{}, true},
		{"AgentStep false", &ir.AgentStep{}, false},
		{"React false", &ir.React{}, false},
		{"Map false", &ir.Map{}, false},
		{"SignalStep false", &ir.SignalStep{}, false},
		{"CallStep false", &ir.CallStep{}, false},
		{"If false", &ir.If{}, false},
		{"Loop false", &ir.Loop{}, false},
		{"Try false", &ir.Try{}, false},
		{"Parallel false", &ir.Parallel{}, false},
		{"Gate false", &ir.Gate{}, false},
		{"Skip false", &ir.Skip{}, false},
		{"Compose false", &ir.Compose{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDeterministicNode(tt.node); got != tt.want {
				t.Errorf("isDeterministicNode(%T) = %v, want %v", tt.node, got, tt.want)
			}
		})
	}
}

func TestComputeNodeKey_Prefix(t *testing.T) {
	k := computeNodeKey("somedigest", nil, nil)
	if !strings.HasPrefix(k, ir.DigestScheme) {
		t.Errorf("key %q does not have prefix %q", k, ir.DigestScheme)
	}
}

func TestComputeNodeKey_OrderIndependence(t *testing.T) {
	d := "awf-d1:sha256:aaaa"

	refs1 := []string{"ref/b", "ref/a", "ref/c"}
	refs2 := []string{"ref/c", "ref/a", "ref/b"}
	pins1 := []string{"pin2", "pin1"}
	pins2 := []string{"pin1", "pin2"}

	k1 := computeNodeKey(d, refs1, pins1)
	k2 := computeNodeKey(d, refs2, pins2)
	if k1 != k2 {
		t.Errorf("order should not matter: k1=%q k2=%q", k1, k2)
	}
}

func TestComputeNodeKey_CallerSlicesNotMutated(t *testing.T) {
	d := "awf-d1:sha256:aaaa"
	refs := []string{"ref/z", "ref/a"}
	pins := []string{"pin/z", "pin/a"}

	origRefs := []string{"ref/z", "ref/a"}
	origPins := []string{"pin/z", "pin/a"}

	computeNodeKey(d, refs, pins)

	for i := range refs {
		if refs[i] != origRefs[i] {
			t.Errorf("refs[%d] mutated: got %q want %q", i, refs[i], origRefs[i])
		}
	}
	for i := range pins {
		if pins[i] != origPins[i] {
			t.Errorf("pins[%d] mutated: got %q want %q", i, pins[i], origPins[i])
		}
	}
}

func TestComputeNodeKey_Sensitivity(t *testing.T) {
	d := "awf-d1:sha256:aaaa"
	refs := []string{"ref/a"}
	pins := []string{"pin1"}

	base := computeNodeKey(d, refs, pins)

	// Change digest
	if k := computeNodeKey("awf-d1:sha256:bbbb", refs, pins); k == base {
		t.Error("changing digest should change key")
	}

	// Add a ref
	if k := computeNodeKey(d, append(refs, "ref/b"), pins); k == base {
		t.Error("adding a ref should change key")
	}

	// Remove ref
	if k := computeNodeKey(d, nil, pins); k == base {
		t.Error("removing refs should change key")
	}

	// Alter a ref
	if k := computeNodeKey(d, []string{"ref/X"}, pins); k == base {
		t.Error("altering a ref should change key")
	}

	// Alter a pin
	if k := computeNodeKey(d, refs, []string{"pin2"}); k == base {
		t.Error("altering a pin should change key")
	}

	// Remove pin
	if k := computeNodeKey(d, refs, nil); k == base {
		t.Error("removing pins should change key")
	}
}

func TestComputeNodeKey_Injectivity_RefSplit(t *testing.T) {
	// ["a","bc"] must NOT collide with ["ab","c"]
	d := "awf-d1:sha256:aaaa"
	k1 := computeNodeKey(d, []string{"a", "bc"}, nil)
	k2 := computeNodeKey(d, []string{"ab", "c"}, nil)
	if k1 == k2 {
		t.Error(`computeNodeKey(d,["a","bc"],nil) == computeNodeKey(d,["ab","c"],nil): framing is not injective`)
	}
}

func TestComputeNodeKey_Injectivity_ListBoundary(t *testing.T) {
	// ["a"] refs + ["b"] pins must NOT collide with ["a","b"] refs + nil pins
	d := "awf-d1:sha256:aaaa"
	k1 := computeNodeKey(d, []string{"a"}, []string{"b"})
	k2 := computeNodeKey(d, []string{"a", "b"}, nil)
	if k1 == k2 {
		t.Error(`computeNodeKey(d,["a"],["b"]) == computeNodeKey(d,["a","b"],nil): list-boundary framing is not injective`)
	}
}

func TestComputeNodeKey_NilAndEmpty(t *testing.T) {
	// nil and empty slice should produce same key (both mean "no items")
	d := "awf-d1:sha256:aaaa"
	k1 := computeNodeKey(d, nil, nil)
	k2 := computeNodeKey(d, []string{}, []string{})
	if k1 != k2 {
		t.Errorf("nil vs empty slice should produce same key: k1=%q k2=%q", k1, k2)
	}
}
