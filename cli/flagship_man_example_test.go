package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// flagshipManPath and flagshipFixturePath are relative to this package's
// directory (cli/), matching the relative-path convention every other
// cli/*_test.go file in this package already uses (e.g.
// cli/examples_validate_test.go's "../examples/**/*.yaml").
//
// The fixture lives under cli/testdata/, NOT examples/, on purpose: it is a
// compose-based workflow, so its adjacent stub compose file
// (testdata/flagship/lab/compose.yml) has to sit in the same directory for the
// os.Root-confined loader to read it. Anything under examples/ is walked by the
// corpus guards (ir/validate_unknown_keys_test.go,
// ir/validate_duration_scalars_test.go), which load every examples/**.{yaml,yml}
// AS A WORKFLOW — a bare compose file's top-level `services:` key would then be
// (correctly) flagged AWF1062, breaking those unrelated guards. testdata/ is not
// walked by anything, so the stub compose stays invisible to the corpus while
// this test still fully validates the fixture (ir.Validate + adapter
// ValidateConfig, a superset of what TestExamplesValidateClean would have done).
const (
	flagshipManPath     = "../man/awf-workflow.5.md"
	flagshipFixturePath = "testdata/flagship/cve-pipeline.yaml"
)

// TestFlagshipManExample_DocSyncAndAdapterValidate is the F1 regression guard.
//
// The man page's format-reference flagship (awf-workflow(5) EXAMPLE section)
// once validated clean under ir.Validate but PERMANENT-FAILED at runtime: its
// `with: { skill:, cve: }` blocks matched no registered adapter's with-schema
// (they predate the claude adapter; the claude adapter requires `prompt`).
// ir.Validate can't catch this because with: is adapter-opaque IR — only the
// resolved adapter's ValidateConfig enforces the with-schema. So the headline
// documentation example taught a shape that could never actually run.
//
// This test closes that gap two ways:
//
//  1. Doc-sync: the flagship code block inside man/awf-workflow.5.md is
//     extracted and asserted BYTE-IDENTICAL (after de-indenting the
//     markdown-code-block's 4-space margin) to the checked-in fixture
//     testdata/flagship/cve-pipeline.yaml. The man page can never again drift
//     from a workflow nobody actually validates.
//  2. Adapter validation: the fixture is loaded, ir.Validate must produce no
//     Error diagnostics, and — the check plain ir.Validate skips — every
//     agent step's resolved adapter must accept its with: via ValidateConfig.
//
// The adapter registry is built with a DUMMY credential
// (ANTHROPIC_API_KEY=dummy) via t.Setenv + buildAgentRegistry (cli/
// agent_registry.go) so this stays fully hermetic: no real credentials, no
// network. This is load-bearing, not decorative — the claude adapter's
// ValidateConfig (agent/claude/validate.go) defaults `bare` to true and
// REJECTS a `prompt`-only with: unless ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN is present in the adapter's env (*ErrBareRequiresAPIKey).
// TestFlagshipManExample_DummyAuthEnvIsLoadBearing proves this by removing the
// dummy env and showing the same call then errors.
func TestFlagshipManExample_DocSyncAndAdapterValidate(t *testing.T) {
	wantErr := checkFlagshipManExample(t, true /* withDummyAuth */)
	if wantErr != nil {
		t.Fatal(wantErr)
	}
}

// TestFlagshipManExample_DummyAuthEnvIsLoadBearing proves the hermeticity fix
// in the test above is actually load-bearing: WITHOUT the dummy
// ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN env, the identical adapter-validate
// step on the identical (fixed) fixture must fail with
// *claude.ErrBareRequiresAPIKey — the claude adapter's bare-mode auth gate.
// If this test ever starts passing, the dummy-env plumbing in the test above
// has silently stopped mattering (e.g. because `bare` stopped defaulting to
// true) and that test's hermetic guarantee needs re-examination.
func TestFlagshipManExample_DummyAuthEnvIsLoadBearing(t *testing.T) {
	err := checkFlagshipManExample(t, false /* withDummyAuth */)
	if err == nil {
		t.Fatal("checkFlagshipManExample with no dummy auth env = nil error, want a bare-requires-API-key rejection (proves the dummy env in the sibling test is load-bearing, not decorative)")
	}
	if !strings.Contains(err.Error(), "bare") && !strings.Contains(err.Error(), "API") {
		t.Fatalf("checkFlagshipManExample with no dummy auth env = %v, want a bare/API-key-shaped rejection", err)
	}
	t.Logf("confirmed load-bearing: without dummy auth env, ValidateConfig rejects with: %v", err)
}

// checkFlagshipManExample runs the full doc-sync + adapter-validate check.
// withDummyAuth controls whether ANTHROPIC_API_KEY=dummy is set on the
// adapter's env before ValidateConfig runs (see the two callers above).
func checkFlagshipManExample(t *testing.T, withDummyAuth bool) error {
	t.Helper()

	// --- 1. doc-sync: man block byte-identical to the fixture ---
	manBytes, err := os.ReadFile(flagshipManPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", flagshipManPath, err)
	}
	deindentedManBlock, err := extractFlagshipBlock(manBytes)
	if err != nil {
		return fmt.Errorf("extract flagship block from %s: %w", flagshipManPath, err)
	}
	fixtureBytes, err := os.ReadFile(flagshipFixturePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", flagshipFixturePath, err)
	}
	if string(deindentedManBlock) != string(fixtureBytes) {
		return fmt.Errorf("man/awf-workflow.5.md flagship EXAMPLE block has drifted from %s (doc-sync failure)\n--- man (de-indented) ---\n%s\n--- fixture ---\n%s",
			flagshipFixturePath, deindentedManBlock, fixtureBytes)
	}

	// --- 2. load + ir.Validate clean (no Error diagnostics) ---
	ld, err := loader.Load(flagshipFixturePath)
	if err != nil {
		return fmt.Errorf("loader.Load(%q): %w", flagshipFixturePath, err)
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		var msgs []string
		for _, d := range diags {
			if d.Severity == ir.Error {
				msgs = append(msgs, fmt.Sprintf("%s at %s: %s", d.Code, d.Path, d.Message))
			}
		}
		return fmt.Errorf("flagship fixture has validation errors: %s", strings.Join(msgs, "; "))
	}

	// --- 3. per-agent-step adapter ValidateConfig, HERMETIC dummy auth ---
	if withDummyAuth {
		t.Setenv("ANTHROPIC_API_KEY", "dummy")
	} else {
		// Make sure no ambient host credential accidentally makes this pass — the
		// whole point is to prove the rejection fires with NO auth present. A var
		// SET to "" would still count as present (buildAgentRegistry/ValidateConfig
		// check map-key presence, not value truthiness — see the doc comment
		// above), so this must be a true Unsetenv, not t.Setenv(key, ""). Mirrors
		// the identical unset-for-real pattern in
		// cli/agent_registry_test.go's TestBuildAgentRegistry_AllowlistedKeyAbsentFromHost_StillRegisters.
		if err := os.Unsetenv("ANTHROPIC_API_KEY"); err != nil {
			return fmt.Errorf("Unsetenv ANTHROPIC_API_KEY: %w", err)
		}
		if err := os.Unsetenv("ANTHROPIC_AUTH_TOKEN"); err != nil {
			return fmt.Errorf("Unsetenv ANTHROPIC_AUTH_TOKEN: %w", err)
		}
	}
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}, container.NewFake())
	if err != nil {
		return fmt.Errorf("buildAgentRegistry: %w", err)
	}

	var walkErr error
	ir.WalkNodes(ld.Workflow.Graph, "", func(n ir.Node, nodePath string) {
		if walkErr != nil {
			return
		}
		step, ok := n.(*ir.AgentStep)
		if !ok {
			return
		}
		ref := engine.AgentRuntimeRef(ld.Workflow, engine.RootModuleID, step.Uses)
		adapter, ok := reg.Lookup(ref)
		if !ok {
			walkErr = fmt.Errorf("step %q: no adapter registered for uses: %q", nodePath, ref)
			return
		}
		if err := adapter.ValidateConfig(step.With); err != nil {
			walkErr = fmt.Errorf("step %q (uses: %s): ValidateConfig: %w", nodePath, ref, err)
		}
	})
	return walkErr
}

// extractFlagshipBlock finds the SINGLE indented (markdown 4-space) code
// block that immediately follows the "# EXAMPLE" heading in the man page and
// returns it with the 4-space margin stripped from every non-blank line
// (blank lines pass through as zero-length lines unchanged) — i.e. exactly
// the bytes a reader would get by copy-pasting the block and dedenting it,
// which is what testdata/flagship/cve-pipeline.yaml must equal byte-for-byte.
//
// Trailing blank lines immediately before the block ends (before prose
// resumes at column 0) are trimmed; a single trailing "\n" is appended so the
// result matches a normally-saved fixture file.
func extractFlagshipBlock(man []byte) ([]byte, error) {
	const heading = "# EXAMPLE"
	const marginPrefix = "    " // markdown indented-code-block margin (4 spaces)

	lines := strings.Split(string(man), "\n")

	headingIdx := -1
	for i, l := range lines {
		if l == heading {
			headingIdx = i
			break
		}
	}
	if headingIdx == -1 {
		return nil, fmt.Errorf("no %q heading found", heading)
	}

	// First indented, non-blank line after the heading starts the code block.
	// Loop while the current line is NOT that start line, i.e. while it lacks the
	// 4-space margin OR is blank (De Morgan of "indented AND non-blank").
	i := headingIdx + 1
	for i < len(lines) && (!strings.HasPrefix(lines[i], marginPrefix) || strings.TrimSpace(lines[i]) == "") {
		i++
	}
	if i >= len(lines) {
		return nil, fmt.Errorf("no indented code block found after %q heading", heading)
	}
	start := i

	// Consume every line that is blank OR still inside the 4-space margin;
	// the first line back at column 0 (prose resuming) ends the block.
	for i < len(lines) && (lines[i] == "" || strings.HasPrefix(lines[i], marginPrefix)) {
		i++
	}
	block := lines[start:i]

	// Trim trailing blank lines picked up just before the block boundary.
	for len(block) > 0 && block[len(block)-1] == "" {
		block = block[:len(block)-1]
	}
	if len(block) == 0 {
		return nil, fmt.Errorf("extracted code block after %q heading is empty", heading)
	}

	dedented := make([]string, len(block))
	for i, l := range block {
		if l == "" {
			dedented[i] = ""
			continue
		}
		if !strings.HasPrefix(l, marginPrefix) {
			return nil, fmt.Errorf("code block line %q lacks the expected %d-space margin", l, len(marginPrefix))
		}
		dedented[i] = l[len(marginPrefix):]
	}

	return []byte(strings.Join(dedented, "\n") + "\n"), nil
}
