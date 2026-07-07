package ir_test

import "testing"

// --- AWF1064: retry.recovery enum ---

func TestRecovery_RestartOK(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    retry:\n      recovery: restart\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1064") {
		t.Fatalf("recovery: restart must load fine, got %v", diags)
	}
}

func TestRecovery_ContinueOK(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    retry:\n      recovery: continue\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1064") {
		t.Fatalf("recovery: continue must load fine, got %v", diags)
	}
}

func TestRecovery_TypoRejected(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    retry:\n      recovery: contineu\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1064") {
		t.Fatalf("expected AWF1064 for recovery typo, got %v", diags)
	}
}

func TestRecovery_UnsetOK(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    retry:\n      attempts: 3\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1064") {
		t.Fatalf("unset recovery must load fine, got %v", diags)
	}
}
