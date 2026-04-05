package ir

import (
	"encoding/json"
	"testing"
)

func TestSeverityString(t *testing.T) {
	if Error.String() != "error" {
		t.Errorf("Error.String() = %q, want %q", Error.String(), "error")
	}
	if Warning.String() != "warning" {
		t.Errorf("Warning.String() = %q, want %q", Warning.String(), "warning")
	}
}

func TestHasErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []Diagnostic
		want bool
	}{
		{"empty", nil, false},
		{"only warnings", []Diagnostic{{Severity: Warning, Code: "AWF2002"}}, false},
		{"one error", []Diagnostic{{Severity: Error, Code: "AWF1003"}}, true},
		{"mixed", []Diagnostic{{Severity: Warning, Code: "AWF2002"}, {Severity: Error, Code: "AWF1003"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasErrors(c.in); got != c.want {
				t.Errorf("HasErrors(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestCatalogCodesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for code, msg := range catalog {
		if prev, dup := seen[code]; dup {
			t.Errorf("code %q duplicated: %q vs %q", code, prev, msg)
		}
		seen[code] = msg
	}
	// Sanity: every code is "AWFNNNN" (uppercase 'AWF' + 4 digits).
	for code := range catalog {
		if len(code) != 7 || code[:3] != "AWF" {
			t.Errorf("code %q does not match AWFNNNN shape", code)
		}
		for _, c := range code[3:] {
			if c < '0' || c > '9' {
				t.Errorf("code %q has non-digit after AWF", code)
			}
		}
	}
}

func TestDiagnosticErrorRenders(t *testing.T) {
	d := Diagnostic{Severity: Error, Path: "graph[0].run", Code: "AWF1005", Message: "image is a tag, not a digest"}
	want := "error AWF1005 at graph[0].run: image is a tag, not a digest"
	if got := d.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestSeverityJSONRoundTrip locks the wire format the slice-1.6 CLI's `--format json` emits:
// severities are STRINGS ("error"/"warning"), not opaque ints. Round-trip via Unmarshal so
// the same byte stream is consumable by another Go process.
func TestSeverityJSONRoundTrip(t *testing.T) {
	d := Diagnostic{Severity: Error, Path: "p", Code: "AWF1003", Message: "m"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Severity":"error","Path":"p","Code":"AWF1003","Message":"m"}`
	if string(b) != want {
		t.Errorf("Marshal = %s, want %s", b, want)
	}
	var d2 Diagnostic
	if err := json.Unmarshal(b, &d2); err != nil {
		t.Fatal(err)
	}
	if d2.Severity != Error {
		t.Errorf("round-trip lost severity: got %v, want Error", d2.Severity)
	}
	// Warning shape
	w := Diagnostic{Severity: Warning, Path: "p", Code: "AWF2002", Message: "m"}
	wb, _ := json.Marshal(w)
	if !contains(string(wb), `"Severity":"warning"`) {
		t.Errorf("Warning marshal lacks the warning string: %s", wb)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
