package agent

import "testing"

// TestExtractJSONObject_Shared pins the exact behavior of the three
// byte-identical per-adapter extractJSONObject copies (awfllm, goose, droid)
// this hoists (F37): fenced JSON, leading prose, a bare object, last-object-wins
// on multiple candidates, string-awareness (braces/escaped-quotes inside string
// values don't fool the brace scan), and the not-found error path.
func TestExtractJSONObject_Shared(t *testing.T) {
	cases := []struct {
		name, in, wantKey string
		wantVal           string
	}{
		{"bare-object", `{"k":"v"}`, "k", "v"},
		{"prose-prefix", `here you go: {"k":"v"}`, "k", "v"},
		{"fenced", "```json\n{\"k\":\"v\"}\n```", "k", "v"},
		{"braces-in-string", `{"k":"has } and { inside"}`, "k", "has } and { inside"},
		{"escaped-quotes", `prefix {"k":"a \"quote\" and {nested}"} suffix`, "k", `a "quote" and {nested}`},
		{"multiple-last-wins", `{"k":"first"} then {"k":"second"}`, "k", "second"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := ExtractJSONObject(c.in)
			if err != nil {
				t.Fatalf("ExtractJSONObject(%q): %v", c.in, err)
			}
			if m[c.wantKey] != c.wantVal {
				t.Errorf("got %v[%q] = %v, want %q", m, c.wantKey, m[c.wantKey], c.wantVal)
			}
		})
	}
	if _, err := ExtractJSONObject("no object here"); err == nil {
		t.Error("ExtractJSONObject(no object): err = nil, want error")
	}
}

// TestExtractJSONObject_FenceAndProse mirrors the pre-hoist awfllm/goose test
// (fenced-JSON-with-trailing-prose) exactly, as a second byte-identity check.
func TestExtractJSONObject_FenceAndProse(t *testing.T) {
	got, err := ExtractJSONObject("here you go:\n```json\n{\"answer\":4}\n```\nthanks")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got["answer"].(float64) != 4 {
		t.Errorf("answer = %v, want 4", got["answer"])
	}
}
