package ir_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// validateForTest / hasCode are defined in validate_unknown_keys_test.go (same
// package ir_test).

func TestDurationScalars_BareIntStepTimeoutRejected(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: echo hi\n    timeout: 300\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1063") {
		t.Fatalf("expected AWF1063 for bare-int timeout, got %v", diags)
	}
}

func TestDurationScalars_QuotedStringAccepted(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: echo hi\n    timeout: \"300s\"\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1063") {
		t.Fatalf("quoted duration must pass, got %v", diags)
	}
}

func TestDurationScalars_BareIntRetryRejected(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: echo hi\n    retry:\n      initial: 5\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1063") {
		t.Fatalf("expected AWF1063 for bare-int retry.initial, got %v", diags)
	}
}

// TestDurationScalars_QuotedRetryAccepted confirms a properly quoted retry.max
// alongside a quoted initial does not false-positive.
func TestDurationScalars_QuotedRetryAccepted(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: echo hi\n    retry:\n      initial: \"1s\"\n      max: \"60s\"\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1063") {
		t.Fatalf("quoted retry durations must pass, got %v", diags)
	}
}

// TestDurationScalars_BareIntToolTimeoutRejected confirms a top-level tools:
// entry's impl.timeout is checked too (tools: is a map, not a graph node list).
func TestDurationScalars_BareIntToolTimeoutRejected(t *testing.T) {
	src := "workflow: x\nversion: 1\n" +
		"containers:\n  box:\n    image: oci://x@sha256:" + strings.Repeat("a", 64) + "\n" +
		"tools:\n" +
		"  greet:\n" +
		"    description: says hi\n" +
		"    input_schema: {type: object}\n" +
		"    impl:\n" +
		"      run: echo hi\n" +
		"      container: box\n" +
		"      timeout: 5\n" +
		"graph:\n  - react:\n      with: {uses: awf/llm}\n      prompt: hi\n      tools: [greet]\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1063") {
		t.Fatalf("expected AWF1063 for bare-int tools.greet.impl.timeout, got %v", diags)
	}
}

// TestDurationScalars_WithSubtreeSkipped confirms a numeric value nested under
// with: is never mistaken for a duration position (with: is opaque and never
// descended into by this pass — it inspects only the specific timeout/retry keys
// of a node's own map, never a generic range over all keys).
func TestDurationScalars_WithSubtreeSkipped(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    with:\n      timeout: 5\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1063") {
		t.Fatalf("with: subtree must be skipped, got %v", diags)
	}
}

// TestDurationScalars_CorpusZeroFalsePositives loads every examples/**/*.yaml and
// asserts ZERO AWF1063 diagnostics. This is the objective safety net: a false
// positive means a corpus workflow has a genuine bare-int duration — a latent bug
// in the corpus workflow (fix it there), not a bug in this pass.
func TestDurationScalars_CorpusZeroFalsePositives(t *testing.T) {
	root := "../examples"
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no example workflows found under %s", root)
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			ld, err := loader.Load(f)
			if err != nil {
				t.Fatalf("loader.Load(%q): %v", f, err)
			}
			for _, d := range ir.Validate(ld) {
				if d.Code == "AWF1063" {
					t.Errorf("false positive AWF1063 in %s at %q: %s", f, d.Path, d.Message)
				}
			}
		})
	}
}
