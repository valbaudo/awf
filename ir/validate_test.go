package ir

import "testing"

// TestValidateSkeletonReturnsEmpty pins that an empty/minimal LoadedDefinition produces no
// diagnostics — the entry point compiles and the no-op skeleton is wired correctly. Real
// rule coverage lands in the per-pass tests added by Tasks 2–5.
func TestValidateSkeletonReturnsEmpty(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID:         "empty",
			Version:    1,
			Containers: map[string]Container{},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/empty.yaml",
		ComposeFiles: map[string][]byte{},
	}
	diags := Validate(ld)
	if len(diags) != 0 {
		t.Errorf("Validate(empty) = %d diagnostics, want 0: %+v", len(diags), diags)
	}
}

// TestValidateNilSafe pins the contract: Validate(nil) does not panic.
func TestValidateNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Validate(nil) panicked: %v", r)
		}
	}()
	diags := Validate(nil)
	// Skeleton returns a single diagnostic for nil so callers can surface it gracefully.
	if !HasErrors(diags) {
		t.Errorf("Validate(nil) should report an error; got %+v", diags)
	}
}
