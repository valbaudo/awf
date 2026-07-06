package ir_test

import "testing"

// --- AWF3015: literal /work/.awf staging path in run:/reduce.run ---

func TestStagingLiteral_RunWithWorkAwf(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: cat /work/.awf/branch-0/out.json\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF3015") {
		t.Fatalf("expected AWF3015 for literal /work/.awf, got %v", diags)
	}
}

func TestStagingLiteral_StagingRootVarOK(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: cat \"$AWF_STAGING_ROOT\"/branch-0/out.json\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF3015") {
		t.Fatalf("$AWF_STAGING_ROOT must not warn, got %v", diags)
	}
}
