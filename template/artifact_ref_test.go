package template

import "testing"

func TestParseArtifactRef(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantID   string
		wantName string
		wantOK   bool
	}{
		{"canonical", "step.recon.files.report", "recon", "report", true},
		{"whitespace trimmed", "  step.a.files.b  ", "a", "b", true},
		{"three segments (missing name)", "step.a.files", "", "", false},
		{"template envelope rejected", "{{ step.a.files.b }}", "", "", false},
		{"non-step root", "run.id", "", "", false},
		{"wrong middle segment", "step.a.outputs.b", "", "", false},
		{"empty", "", "", "", false},
		{"five segments", "step.a.files.b.c", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, name, ok := ParseArtifactRef(tc.raw)
			if ok != tc.wantOK || id != tc.wantID || name != tc.wantName {
				t.Errorf("ParseArtifactRef(%q) = (%q, %q, %v); want (%q, %q, %v)",
					tc.raw, id, name, ok, tc.wantID, tc.wantName, tc.wantOK)
			}
		})
	}
}
