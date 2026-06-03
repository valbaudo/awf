package cli_test

import (
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// TestExamplesValidateClean loads every *.yaml file under examples/ and asserts
// that ir.Validate produces no Error diagnostics. This acts as an automated guard:
// adding a new example or changing an existing one that breaks the validator will
// surface here immediately rather than silently shipping a broken workflow.
//
// Warnings (e.g. AWF3006) are permitted — they are informational and don't indicate
// a broken workflow. Only Errors contribute to HasErrors and fail a run.
func TestExamplesValidateClean(t *testing.T) {
	matches, err := filepath.Glob("../examples/**/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Also glob top-level *.yaml in case examples are not nested.
	top, err := filepath.Glob("../examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	all := append(matches, top...)
	// Deduplicate (Glob returns sorted, non-overlapping for distinct patterns, but the
	// two patterns could overlap if examples/ itself had *.yaml files — keep it safe).
	seen := map[string]bool{}
	unique := all[:0]
	for _, p := range all {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		if !seen[abs] {
			seen[abs] = true
			unique = append(unique, p)
		}
	}
	if len(unique) == 0 {
		t.Fatal("no *.yaml files found under examples/ — check the glob pattern")
	}
	for _, fixture := range unique {
		fixture := fixture
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			ld, err := loader.Load(fixture)
			if err != nil {
				t.Fatalf("loader.Load(%q): %v", fixture, err)
			}
			diags := ir.Validate(ld)
			if ir.HasErrors(diags) {
				for _, d := range diags {
					if d.Severity == ir.Error {
						t.Errorf("example %s produced Error %s at %s: %s", fixture, d.Code, d.Path, d.Message)
					}
				}
				t.Fatalf("example workflow %s has validation errors — fix the workflow or the validator", fixture)
			}
		})
	}
}
