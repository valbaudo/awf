package obs

import "testing"

func TestSpanStatusString(t *testing.T) {
	if StatusOK == StatusError {
		t.Fatal("status constants must be distinct")
	}
	s := Span{Path: "triage", Kind: "agent", Status: StatusOK, Attributes: map[string]any{"awf.node.path": "triage"}}
	if s.Attributes["awf.node.path"] != "triage" {
		t.Fatalf("attribute round-trip failed")
	}
}
