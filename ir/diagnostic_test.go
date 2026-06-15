package ir

import (
	"encoding/json"
	"strings"
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
		if strings.HasPrefix(code, "AWF_IMPORT_") {
			continue
		}
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

func TestCatalogIncludesLoaderDiagnosticCodes(t *testing.T) {
	for _, code := range []string{
		"AWF_IMPORT_CYCLE",
		"AWF_IMPORT_DECODE",
		"AWF_IMPORT_DEPTH",
		"AWF_IMPORT_ID_INVALID",
		"AWF_IMPORT_PATH_ABSOLUTE",
		"AWF_IMPORT_PATH_BACKSLASH",
		"AWF_IMPORT_PATH_ESCAPE",
		"AWF_IMPORT_PATH_INVALID",
		"AWF_IMPORT_READ",
		"AWF_IMPORT_SYMLINK",
	} {
		if _, ok := catalog[code]; !ok {
			t.Errorf("catalog missing loader diagnostic code %s", code)
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

func TestDiagnosticSourceRenders(t *testing.T) {
	cases := []struct {
		name string
		in   Diagnostic
		want string
	}{
		{
			name: "source-and-path",
			in:   Diagnostic{Severity: Error, Source: "/tmp/child.awf.yaml", Path: "graph[0].run", Code: "AWF1005", Message: "image is a tag"},
			want: "error AWF1005 at /tmp/child.awf.yaml:graph[0].run: image is a tag",
		},
		{
			name: "source-only",
			in:   Diagnostic{Severity: Error, Source: "/tmp/child.awf.yaml", Code: "AWF_IMPORT_READ", Message: "read workflow"},
			want: "error AWF_IMPORT_READ at /tmp/child.awf.yaml: read workflow",
		},
		{
			name: "root-shape-unchanged",
			in:   Diagnostic{Severity: Error, Code: "AWF1003", Message: "nil or empty LoadedDefinition"},
			want: "error AWF1003: nil or empty LoadedDefinition",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
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
	want := `{"severity":"error","path":"p","code":"AWF1003","message":"m"}`
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
	if !contains(string(wb), `"severity":"warning"`) {
		t.Errorf("Warning marshal lacks the warning string: %s", wb)
	}
}

// TestDiagnosticJSONFieldNames locks the all-lowercase wire keys (S3). `awf validate --output
// json` is the primary consumer; lowercase keys let `jq '.diagnostics[].code'` work without a
// case-special-casing dance. Field order follows struct declaration (severity, source, path,
// code, message). Source carries omitempty: present (as "source") when set, absent when empty.
func TestDiagnosticJSONFieldNames(t *testing.T) {
	full := Diagnostic{Severity: Error, Source: "child.yaml", Path: "graph[0]", Code: "AWF1005", Message: "m"}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"severity":"error","source":"child.yaml","path":"graph[0]","code":"AWF1005","message":"m"}`
	if string(b) != want {
		t.Errorf("Marshal(full) = %s, want %s", b, want)
	}
	// Source is omitempty: the key is absent when the field is empty.
	bare, err := json.Marshal(Diagnostic{Severity: Warning, Path: "p", Code: "AWF2002", Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(bare), "source") {
		t.Errorf("empty Source should be omitted, got %s", bare)
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
