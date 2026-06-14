package ir_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

var update = flag.Bool("update", false, "regenerate .golden files for invalid fixtures")

// primaryCodeOf extracts "AWF1004" from the fixture filename "AWF1004-duplicate-step-id.yaml".
// The test asserts this code is present in the diagnostic stream — independently of the byte-
// exact golden comparison. Without this anchor, regenerating the golden via `-update` would
// turn the test into a self-affirming tautology.
func primaryCodeOf(fixturePath string) string {
	base := filepath.Base(fixturePath)
	if i := strings.Index(base, "-"); i > 0 {
		return base[:i]
	}
	return ""
}

func TestInvalidFixturesGolden(t *testing.T) {
	matches, err := filepath.Glob("testdata/invalid/AWF*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no fixtures matched testdata/invalid/AWF*.yaml — did you create them?")
	}
	for _, fixture := range matches {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			ld, err := loader.Load(fixture)
			if err != nil {
				t.Fatalf("load(%q): %v", fixture, err)
			}
			diags := ir.Validate(ld)
			// (a) Primary-code anchor: the expected code MUST appear in the diagnostic stream.
			// This prevents `-update` from turning the test into a self-affirming tautology.
			wantCode := primaryCodeOf(fixture)
			found := false
			for _, d := range diags {
				if d.Code == wantCode {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("fixture %s expected primary code %q, not present in diagnostics: %+v", fixture, wantCode, diags)
			}
			// (b) Byte-exact golden comparison.
			// Strip any absolute-path noise from messages so goldens are portable.
			scrub := func(d ir.Diagnostic) ir.Diagnostic {
				d.Message = strings.ReplaceAll(d.Message, ld.WorkflowPath, "<workflow>")
				return d
			}
			out := make([]ir.Diagnostic, len(diags))
			for i, d := range diags {
				out[i] = scrub(d)
			}
			gotJSON, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := strings.TrimSuffix(fixture, ".yaml") + ".golden"
			if *update {
				if err := os.WriteFile(golden, append(gotJSON, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantBytes, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden %q: %v (run with -update to create)", golden, err)
			}
			want := strings.TrimSpace(string(wantBytes))
			got := strings.TrimSpace(string(gotJSON))
			if want != got {
				t.Errorf("diagnostics differ for %s\nwant:\n%s\n\ngot:\n%s", fixture, want, got)
			}
		})
	}
}

// TestValidFixturePassesClean asserts the slice-1.2 cve-pipeline.yaml fixture loads + validates
// with ZERO Error diagnostics. (Warnings are OK and tracked separately.)
func TestValidFixturePassesClean(t *testing.T) {
	ld, err := loader.Load("../loader/testdata/valid/cve-pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	diags := ir.Validate(ld)
	for _, d := range diags {
		if d.Severity == ir.Error {
			t.Errorf("valid fixture should produce no Error: %v", d)
		}
	}
}

// A containerless awf/llm step keys input_files by a logical LABEL ("doc"); a bare
// label must NOT be rejected as a non-absolute container path (AWF3007).
func TestInputFiles_ContainerlessLabelKeyAccepted(t *testing.T) {
	ld, err := loader.Load("testdata/containerless_input_files_label.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ir.Validate(ld) {
		if d.Code == "AWF3007" && d.Severity == ir.Error {
			t.Fatalf("label key wrongly rejected: %v", d)
		}
	}
}

// A container-backed step still requires an absolute, clean input_files key — a
// bare relative label like "doc" must produce AWF3007.
func TestInputFiles_ContainerBackedStillRequiresAbsPath(t *testing.T) {
	ld, err := loader.Load("testdata/container_input_files_relative.yaml")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range ir.Validate(ld) {
		if d.Code == "AWF3007" && d.Severity == ir.Error {
			found = true
		}
	}
	if !found {
		t.Fatal("container-backed relative key should be rejected with AWF3007")
	}
}
