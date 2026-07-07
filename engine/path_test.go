package engine

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestIterPath(t *testing.T) {
	cases := []struct {
		body string
		iter int
		want string
	}{
		{"loop[0].body", 1, "loop[0].body.iter-1"},
		{"loop[0].body", 3, "loop[0].body.iter-3"},
		{"if[1].then.loop[0].body", 2, "if[1].then.loop[0].body.iter-2"},
	}
	for _, c := range cases {
		if got := IterPath(c.body, c.iter); got != c.want {
			t.Errorf("IterPath(%q, %d) = %q, want %q", c.body, c.iter, got, c.want)
		}
	}
}

func TestAttemptPath(t *testing.T) {
	cases := []struct {
		gate    string
		attempt int
		want    string
	}{
		{"gate[0]", 1, "gate[0].attempt-1"},
		{"gate[0]", 2, "gate[0].attempt-2"},
		{"try[0].do.gate[1]", 5, "try[0].do.gate[1].attempt-5"},
	}
	for _, c := range cases {
		if got := AttemptPath(c.gate, c.attempt); got != c.want {
			t.Errorf("AttemptPath(%q, %d) = %q, want %q", c.gate, c.attempt, got, c.want)
		}
	}
}

func TestItemPath(t *testing.T) {
	cases := []struct {
		m    string
		item int
		want string
	}{
		{"map[0]", 0, "map[0].item-0"},
		{"map[0]", 3, "map[0].item-3"},
		{"parallel[2].map[0]", 7, "parallel[2].map[0].item-7"},
	}
	for _, c := range cases {
		if got := ItemPath(c.m, c.item); got != c.want {
			t.Errorf("ItemPath(%q, %d) = %q, want %q", c.m, c.item, got, c.want)
		}
	}
}

func TestParentPath(t *testing.T) {
	cases := []struct {
		child      string
		wantParent string
		wantOK     bool
	}{
		{"triage", "", false},                                                      // top-level step → root (no parent segment)
		{"gate[0]", "", false},                                                     // top-level control node → root
		{"gate[0].attempt-2", "gate[0]", true},                                     // attempt scope → gate
		{"gate[0].attempt-2.generate", "gate[0].attempt-2", true},                  // branch → attempt
		{"gate[0].attempt-2.generate.exploit", "gate[0].attempt-2.generate", true}, // step → branch
		{"if[1].then", "if[1]", true},                                              // branch → if
		{"if[1].then.foo", "if[1].then", true},                                     // step → branch
		{"loop[0].body", "loop[0]", true},                                          // body → loop
		{"loop[0].body.iter-3", "loop[0].body", true},                              // iteration → body
		{"loop[0].body.iter-3.bar", "loop[0].body.iter-3", true},                   // step → iteration
		{"map[0].item-0", "map[0]", true},                                          // item → map
		{"parallel[2].baz", "parallel[2]", true},                                   // step → parallel branch
		{"", "", false},                                                            // root has no parent
	}
	for _, c := range cases {
		gotParent, gotOK := ParentPath(c.child)
		if gotParent != c.wantParent || gotOK != c.wantOK {
			t.Errorf("ParentPath(%q) = (%q, %v), want (%q, %v)", c.child, gotParent, gotOK, c.wantParent, c.wantOK)
		}
	}
}

func TestRoundPathHelpers(t *testing.T) {
	r := "react[0]"
	rp := RoundPath(r, 2)
	if rp != "react[0].round-2" {
		t.Fatalf("RoundPath = %q, want react[0].round-2", rp)
	}
	if ModelPath(rp) != "react[0].round-2.model" {
		t.Fatalf("ModelPath = %q", ModelPath(rp))
	}
	if ToolPath(rp, 1) != "react[0].round-2.tool-1" {
		t.Fatalf("ToolPath = %q", ToolPath(rp, 1))
	}
	// ParentPath already handles the new segments (trims the last '.'-segment).
	gotParent, ok := ParentPath(ModelPath(rp))
	if !ok || gotParent != rp {
		t.Fatalf("ParentPath(model) = (%q, %v), want (%q, true)", gotParent, ok, rp)
	}
}

// Spec §4 round-trip: compose ir.PathFor (static prefix) with engine.IterPath (runtime suffix).
// This pins the contract that the validator's static path is the prefix of the runtime path.
func TestStaticPlusRuntime_Compose(t *testing.T) {
	// loop[0].body.iter-3.exploit — a step "exploit" inside the 3rd iteration of the top-level loop.
	loopPath := ir.PathFor("", "loop", "", 0)          // "loop[0]"
	bodyPath := ir.ChildPath("", "loop", 0, "body")    // "loop[0].body"
	iterPath := IterPath(bodyPath, 3)                  // "loop[0].body.iter-3"
	stepPath := ir.PathFor(iterPath, "", "exploit", 0) // "loop[0].body.iter-3.exploit"

	if loopPath != "loop[0]" {
		t.Errorf("loopPath = %q", loopPath)
	}
	if bodyPath != "loop[0].body" {
		t.Errorf("bodyPath = %q", bodyPath)
	}
	if iterPath != "loop[0].body.iter-3" {
		t.Errorf("iterPath = %q", iterPath)
	}
	if stepPath != "loop[0].body.iter-3.exploit" {
		t.Errorf("stepPath = %q", stepPath)
	}
}

// TestBareRunHandleKey pins F4a's reserved-key scheme: keyed by node path
// (so two bare steps never collide), and provably distinct from every real
// QualifiedContainerKey result (declared containers can't contain ':' —
// ir.containerNamePattern — and a qualified key only ever inserts "::").
func TestBareRunHandleKey(t *testing.T) {
	cases := []struct {
		nodePath string
		want     string
	}{
		{"build", "_run:build"},
		{"parallel[0].b0", "_run:parallel[0].b0"},
		{"parallel[0].b1", "_run:parallel[0].b1"},
	}
	for _, c := range cases {
		if got := BareRunHandleKey(c.nodePath); got != c.want {
			t.Errorf("BareRunHandleKey(%q) = %q, want %q", c.nodePath, got, c.want)
		}
	}

	// Two distinct node paths never collide.
	if BareRunHandleKey("parallel[0].b0") == BareRunHandleKey("parallel[0].b1") {
		t.Error("BareRunHandleKey collision across distinct node paths")
	}

	// Never collides with a plain declared-container key (no runtimeParent).
	for _, container := range []string{"lab", "_run", "build"} {
		if BareRunHandleKey("x") == QualifiedContainerKey("", container) {
			t.Errorf("BareRunHandleKey collides with QualifiedContainerKey(\"\", %q)", container)
		}
	}
	// Never collides with a call-scoped ("::"-qualified) declared-container key.
	if BareRunHandleKey("x") == QualifiedContainerKey("build.workflow", "lab") {
		t.Error("BareRunHandleKey collides with a call-scoped QualifiedContainerKey")
	}
}
