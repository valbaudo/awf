package template

import (
	"testing"
)

// dumpRefs maps dumpRef over a slice of refs. The single-ref form lives in parser_test.go.
func dumpRefs(refs []Ref) []string {
	out := make([]string, 0, len(refs))
	for i := range refs {
		out = append(out, dumpRef(&refs[i]))
	}
	return out
}

func TestReferences(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"single ref", "step.triage.web_exploitable", []string{"step.triage.web_exploitable"}},
		{"comparison both sides", "step.x.a == step.y.b", []string{"step.x.a", "step.y.b"}},
		{"literal vs ref", `"hello" == step.x.out`, []string{"step.x.out"}},
		{"all-literal: no refs", "true && false", nil},
		{
			"appendix-A gate condition",
			"evaluate.verified && evaluate.detections == 5 && evaluate.false_positives == 0",
			[]string{"evaluate.verified", "evaluate.detections", "evaluate.false_positives"},
		},
		{
			"appendix-A skip-guard",
			"!(step.triage.web_exploitable && step.triage.has_source)",
			[]string{"step.triage.web_exploitable", "step.triage.has_source"},
		},
		{"order is left-to-right", "(a.x || b.y) && c.z", []string{"a.x", "b.y", "c.z"}},
		{"duplicate refs preserved", "a.x && a.x", []string{"a.x", "a.x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", c.src, err)
			}
			got := dumpRefs(References(e))
			if len(got) != len(c.want) {
				t.Fatalf("References(%q) = %v, want %v", c.src, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("ref[%d] = %q, want %q (full: %v vs %v)", i, got[i], c.want[i], got, c.want)
				}
			}
		})
	}
}

func TestReferencesPreservesSegmentPos(t *testing.T) {
	// Per-segment positions are preserved so slice 1.4 can point at a specific failing
	// segment (e.g. `step.triage.field` → "triage" unknown). The first segment's position
	// is the "start of ref" — callers use Segments[0].Pos for that.
	e, err := ParseExpr("step.triage.web_exploitable && other.x")
	if err != nil {
		t.Fatal(err)
	}
	refs := References(e)
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
	// First ref: step.triage.web_exploitable starting at offset 0.
	// segments[0]=step @ 0, segments[1]=triage @ 5, segments[2]=web_exploitable @ 12.
	wantPos := []int{0, 5, 12}
	for i, want := range wantPos {
		if refs[0].Segments[i].Pos != want {
			t.Errorf("refs[0].Segments[%d].Pos = %d, want %d (ident=%q)", i, refs[0].Segments[i].Pos, want, refs[0].Segments[i].Ident)
		}
	}
	// Second ref: other.x starting at offset 31 ("step.triage.web_exploitable && " is 31 chars).
	if refs[1].Segments[0].Pos != 31 {
		t.Errorf("refs[1].Segments[0].Pos = %d, want 31", refs[1].Segments[0].Pos)
	}
	if refs[1].Segments[1].Pos != 37 {
		t.Errorf("refs[1].Segments[1].Pos = %d, want 37", refs[1].Segments[1].Pos)
	}
}
