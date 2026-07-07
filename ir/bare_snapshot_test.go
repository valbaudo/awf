package ir_test

import "testing"

// F4b (e): a bare `run:` step (no declared container:) + `snapshot: workspace`
// is structurally UNEXPRESSIBLE on the wire — not merely rejected by a new
// validate rule. `snapshot:` is a property of a NAMED entry under the
// top-level `containers:` map (ir.Container.Snapshot); *ir.CodeStep's own
// schema has no `snapshot` field at all (see ir/node.go), and a bare step's
// empty `container:` means it has no reference into `containers:` to attach
// one via indirection either. So there is no way to write "bare step +
// snapshot: workspace" for the SAME step in the first place:
//
//   - Writing `snapshot: workspace` directly on a step (sibling of `run:`) is
//     caught by the EXISTING generic unknown-key validator, AWF1062 (below) —
//     CodeStep's json-tagged field set doesn't include it.
//   - Writing it on an actual `containers:` entry only ever affects a step
//     that references that container by name via `container:` — which makes
//     the step non-bare by definition, not the case this task is about.
//
// So ir/validate_structural.go needs NO new rejection rule for this — the
// existing AWF1062 unknown-key pass already closes it structurally.
// engine/interpreter.go's resolution (a bare step's Snapshot is read via
// wf.Containers[""].Snapshot, which is always a Go zero-value map miss since
// containerNamePattern forbids an empty container name) reinforces this at
// the runtime-resolution layer, independent of the schema-level proof here.
func TestBareSnapshotWorkspaceKeyOnBareStepIsUnknownKey(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - id: bare\n    run: echo hi\n    snapshot: workspace\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1062") {
		t.Fatalf("expected AWF1062 (snapshot: is not a CodeStep field) for a bare step declaring snapshot: workspace directly, got %v", diags)
	}
}

// A DECLARED container's own snapshot: workspace still validates cleanly —
// confirms the AWF1062 hit above is specifically about attaching snapshot: to
// a bare STEP, not a blanket rejection of the snapshot: key.
func TestBareSnapshotWorkspaceOnDeclaredContainerStillValid(t *testing.T) {
	src := "workflow: x\nversion: 1\ncontainers:\n  lab:\n    image: oci://example.com/lab@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"\n    snapshot: workspace\ngraph:\n  - id: declared\n    container: lab\n    run: echo hi\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("declared-container snapshot: workspace must not be flagged AWF1062, got %v", diags)
	}
}
