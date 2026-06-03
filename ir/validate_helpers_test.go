package ir

import "testing"

// Shared test helpers for the validator. The five validate_*_test.go files all consume
// these — keep them together so per-pass test files stay focused on rules, not infrastructure.

// assertOneError asserts diags contains exactly one Error of the given code anywhere in the
// slice. Use when the test wants to catch double-emission of the same code.
func assertOneError(t *testing.T, diags []Diagnostic, code string) {
	t.Helper()
	count := 0
	for _, d := range diags {
		if d.Code == code && d.Severity == Error {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly 1 Error with code %q; got %d in %+v", code, count, diags)
	}
}

// assertErrorAt asserts diags contains an Error with the given code at the EXACT given path.
// Exact-match (not substring) so short path components like "a" don't collide with substrings
// of unrelated paths (e.g. "containers.lab.image" contains "a" inside "lab").
func assertErrorAt(t *testing.T, diags []Diagnostic, code, exactPath string) {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && d.Severity == Error && d.Path == exactPath {
			return
		}
	}
	t.Errorf("want Error %q at path %q in %+v", code, exactPath, diags)
}

// makeLD wraps a Workflow in a minimal LoadedDefinition (no compose files). Used by tests
// that exercise the structural / refs / schema passes — the compose pass tests construct
// LoadedDefinition literals directly to populate ComposeFiles.
func makeLD(wf *Workflow) *LoadedDefinition {
	return &LoadedDefinition{
		Workflow:     wf,
		WorkflowPath: "/tmp/test.yaml",
		ComposeFiles: map[string][]byte{},
	}
}

func intPtr(n int) *int { return &n }

// assertNoErrorCode asserts diags contains NO Error with the given code anywhere.
// Used by "this reference is allowed" tests that tolerate unrelated diagnostics
// but must not trip the rule under test.
func assertNoErrorCode(t *testing.T, diags []Diagnostic, code string) {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && d.Severity == Error {
			t.Errorf("did not expect Error %q; got %+v", code, d)
		}
	}
}
