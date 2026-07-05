package agent

import "testing"

func TestPrependFeedback_ByteIdentical(t *testing.T) {
	fb := map[string]any{"passed": false, "reason": "missing X"}
	got, err := PrependFeedback("do the thing", fb)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// The exact legacy form: "<previous verdict>\n<json>\n\n<prompt>".
	want := "<previous verdict>\n{\"passed\":false,\"reason\":\"missing X\"}\n\ndo the thing"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// Empty feedback → prompt unchanged.
	if g, _ := PrependFeedback("p", nil); g != "p" {
		t.Fatalf("empty feedback changed prompt: %q", g)
	}
	if g, _ := PrependFeedback("p", map[string]any{}); g != "p" {
		t.Fatalf("empty-map feedback changed prompt: %q", g)
	}
}
