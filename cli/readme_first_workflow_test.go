package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// readmePath and readmeFirstWorkflowFixturePath are relative to this
// package's directory (cli/), matching the relative-path convention every
// other cli/*_test.go file in this package already uses (see
// cli/flagship_man_example_test.go's flagshipManPath/flagshipFixturePath).
//
// The fixture lives under cli/testdata/, NOT examples/, mirroring F1's
// flagship fixture placement: examples/ is walked wholesale by the corpus
// guards (cli/examples_validate_test.go, ir/validate_unknown_keys_test.go,
// ir/validate_duration_scalars_test.go) as a directory of user-facing runnable
// demos, not README-prose fixtures. testdata/ is not walked by anything, so
// this fixture stays invisible to that corpus while this test still fully
// validates it (ir.Validate, a superset of what the corpus guard does, plus
// the backend-routing checks below that the corpus guard doesn't perform).
const (
	readmePath                     = "../README.md"
	readmeFirstWorkflowFixturePath = "testdata/readme/hello-world.yaml"
)

// TestReadmeHelloWorld_DocSyncValidatesAndRoutesNative is the F9
// regression guard.
//
// The README's "Get Started" section (F9) was rewritten from a
// gated release-note example that silently required an unstated Ollama
// server + OPENAI_API_KEY (the F9 gap) into a genuine zero-setup, 4-key
// bare-`run:` hello-world that F4 (container: optional on run: code steps)
// makes possible. Prose in that section makes three specific claims:
//
//  1. the exact YAML shown loads and validates cleanly;
//  2. it needs no container/image/model/key because `auto` backend
//     selection routes it to `native` (no Docker-only feature present);
//  3. an explicit `--backend docker` refuses it (AWF1065) — the hello-world
//     relies on auto/native, not docker.
//
// This test closes the F1 failure mode (a README/man example that reads
// clean but never actually works) for this new example by proving all
// three claims against the ACTUAL fenced ```yaml block in README.md, not a
// hand-copied approximation of it:
//
//  1. Doc-sync: the first ```yaml fenced block following the
//     "## Get Started" heading is extracted and asserted
//     byte-identical to the checked-in fixture
//     testdata/readme/hello-world.yaml. The README can never again drift
//     from a workflow nobody actually validates.
//  2. ir.Validate produces no Error diagnostics, and
//     selectRunBackendForLoadedDefinition(backendAuto, ...) resolves to
//     engine.BackendNative — proving the "auto picks native" claim, not
//     just "it happens not to need docker".
//  3. checkContainerlessRunCapability(ld, engine.BackendDocker) returns a
//     non-nil AWF1065 error — proving the "--backend docker refuses it"
//     claim is accurate, not overclaimed.
func TestReadmeHelloWorld_DocSyncValidatesAndRoutesNative(t *testing.T) {
	// --- 1. doc-sync: README block byte-identical to the fixture ---
	readmeBytes, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}
	block, err := extractReadmeFirstWorkflowBlock(readmeBytes)
	if err != nil {
		t.Fatalf("extract first-workflow block from %s: %v", readmePath, err)
	}
	fixtureBytes, err := os.ReadFile(readmeFirstWorkflowFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmeFirstWorkflowFixturePath, err)
	}
	if string(block) != string(fixtureBytes) {
		t.Fatalf("README.md \"Get Started\" block has drifted from %s (doc-sync failure)\n--- README ---\n%s\n--- fixture ---\n%s",
			readmeFirstWorkflowFixturePath, block, fixtureBytes)
	}

	// --- 2. load + ir.Validate clean (no Error diagnostics) ---
	ld, err := loader.Load(readmeFirstWorkflowFixturePath)
	if err != nil {
		t.Fatalf("loader.Load(%q): %v", readmeFirstWorkflowFixturePath, err)
	}
	diags := ir.Validate(ld)
	if ir.HasErrors(diags) {
		var msgs []string
		for _, d := range diags {
			if d.Severity == ir.Error {
				msgs = append(msgs, fmt.Sprintf("%s at %s: %s", d.Code, d.Path, d.Message))
			}
		}
		t.Fatalf("README hello-world fixture has validation errors: %s", strings.Join(msgs, "; "))
	}

	// --- 3. "no container/image/model/key needed" == auto routes native ---
	backendKind, err := selectRunBackendForLoadedDefinition(backendAuto, ld)
	if err != nil {
		t.Fatalf("selectRunBackendForLoadedDefinition(auto, ...): %v", err)
	}
	if backendKind != engine.BackendNative {
		t.Fatalf("selectRunBackendForLoadedDefinition(auto, ...) = %q, want %q (README claims a bare `run:` hello-world auto-routes to native)", backendKind, engine.BackendNative)
	}

	// --- 4. accurate not-overclaimed: --backend docker refuses it (AWF1065) ---
	dockerErr := checkContainerlessRunCapability(ld, engine.BackendDocker)
	if dockerErr == nil {
		t.Fatal("checkContainerlessRunCapability(ld, docker) = nil, want an AWF1065 rejection (README states --backend docker refuses a bare run: step)")
	}
	if !strings.Contains(dockerErr.Error(), "AWF1065") {
		t.Fatalf("checkContainerlessRunCapability(ld, docker) = %v, want an AWF1065-tagged error", dockerErr)
	}
}

// extractReadmeFirstWorkflowBlock finds the "## Get Started" heading in
// README.md and returns the bytes of the FIRST fenced ```yaml code block that
// follows it, verbatim (README's top-level fenced blocks carry no markdown
// margin to strip, unlike the man page's indented-code-block EXAMPLE section —
// see extractFlagshipBlock in cli/flagship_man_example_test.go for that
// different case). The returned bytes end in a single trailing "\n", matching
// a normally-saved fixture file.
func extractReadmeFirstWorkflowBlock(readme []byte) ([]byte, error) {
	const heading = "## Get Started"
	const fenceYAML = "```yaml"
	const fenceEnd = "```"

	lines := strings.Split(string(readme), "\n")

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

	fenceStart := -1
	for i := headingIdx + 1; i < len(lines); i++ {
		if lines[i] == fenceYAML {
			fenceStart = i
			break
		}
	}
	if fenceStart == -1 {
		return nil, fmt.Errorf("no %q fence found after %q heading", fenceYAML, heading)
	}

	fenceClose := -1
	for i := fenceStart + 1; i < len(lines); i++ {
		if lines[i] == fenceEnd {
			fenceClose = i
			break
		}
	}
	if fenceClose == -1 {
		return nil, fmt.Errorf("%q fence opened at line %d never closes", fenceYAML, fenceStart+1)
	}

	block := lines[fenceStart+1 : fenceClose]
	if len(block) == 0 {
		return nil, fmt.Errorf("fenced block after %q heading is empty", heading)
	}
	return []byte(strings.Join(block, "\n") + "\n"), nil
}
