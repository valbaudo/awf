package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// writeFixture writes content to a tmp YAML file under t.TempDir() and returns the absolute
// path. Hermetic — each test gets its own dir. Avoids coupling unit tests to the cross-package
// loader/testdata or ir/testdata trees (golden integration tests do that, in Task 2).
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// validMinimal is the simplest workflow that passes every slice-1.4 validator. One
// digest-pinned image-backed container, one code step referencing it. No compose file (the
// digest-fold path is exercised in Task 2's golden tests against the real cve-pipeline).
const validMinimal = `workflow: smoke
version: 1
containers:
  c:
    image: oci://example.com/x@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: a
    container: c
    run: ./a.sh
`

// invalidDupID triggers AWF1004 (duplicate step id). Smallest invalid fixture that surfaces
// a single error diagnostic.
const invalidDupID = `workflow: dup
version: 1
containers:
  c:
    image: oci://example.com/x@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: a
    container: c
    run: ./a.sh
  - id: a
    container: c
    run: ./b.sh
`

func TestRunNoArgsExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(nil, &stdout, &stderr); code != ExitUsage {
		t.Errorf("Run(nil) = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage line on stderr; got %q", stderr.String())
	}
}

func TestRunUnknownSubcommandExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"banana"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("Run(banana) = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' on stderr; got %q", stderr.String())
	}
}

func TestRunHelpExitsOK(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{arg}, &stdout, &stderr); code != ExitOK {
			t.Errorf("Run(%q) = %d, want %d", arg, code, ExitOK)
		}
		if !strings.Contains(stdout.String(), "usage:") {
			t.Errorf("Run(%q): expected usage on stdout; got %q", arg, stdout.String())
		}
	}
}

func TestValidateNoPathExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("Run(validate) = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage line on stderr; got %q", stderr.String())
	}
}

func TestValidateHelpFlagExitsOK(t *testing.T) {
	// Go's flag package returns flag.ErrHelp for `-h`, `--help`, AND `-help` (single dash,
	// full word). All three must exit 0 with usage on stdout — a string-search pre-filter
	// would miss `-help`, so cliValidate uses errors.Is(err, flag.ErrHelp).
	for _, arg := range []string{"-h", "--help", "-help"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"validate", arg}, &stdout, &stderr); code != ExitOK {
			t.Errorf("Run(validate %q) = %d, want %d (stderr=%q)", arg, code, ExitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "usage:") {
			t.Errorf("Run(validate %q): expected usage on stdout; got %q", arg, stdout.String())
		}
		// Help is never an error — stderr must be silent so `awf validate -h 2>/dev/null` works.
		if stderr.Len() > 0 {
			t.Errorf("Run(validate %q): unexpected stderr: %q", arg, stderr.String())
		}
	}
}

func TestValidateBadFlagExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--bogus"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("Run(validate --bogus) = %d, want %d", code, ExitUsage)
	}
	// Error message attributes via "awf validate:" prefix, then usage follows on stderr.
	if !strings.Contains(stderr.String(), "awf validate:") {
		t.Errorf("expected 'awf validate:' attribution on stderr; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage line on stderr; got %q", stderr.String())
	}
}

func TestValidateUnknownFormatExitsUsage(t *testing.T) {
	path := writeFixture(t, "wf.yaml", validMinimal)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--format", "xml", path}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("Run(validate --format xml) = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown --format") {
		t.Errorf("expected 'unknown --format' on stderr; got %q", stderr.String())
	}
}

func TestValidateNonexistentFileExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "/no/such/file.yaml"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("Run(validate /no/such/file) = %d, want %d", code, ExitUsage)
	}
	// Error message attribution should mention the path.
	if !strings.Contains(stderr.String(), "/no/such/file.yaml") {
		t.Errorf("expected stderr to mention path; got %q", stderr.String())
	}
}

func TestValidateCleanWorkflowExitsOKText(t *testing.T) {
	path := writeFixture(t, "wf.yaml", validMinimal)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", path}, &stdout, &stderr)
	if code != ExitOK {
		t.Errorf("Run = %d, want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, ": ok") {
		t.Errorf("expected ': ok' line on stdout; got %q", out)
	}
	if !strings.Contains(out, "digest: awf-d1:sha256:") {
		t.Errorf("expected 'digest:' line with awf-d1:sha256: prefix; got %q", out)
	}
	// Anti-regression: a future bug that prints debug or "info" lines to stderr on the
	// clean path would silently pass every other test. One stderr-empty assertion per
	// output format (this one + TestValidateCleanWorkflowJSONExitsOK below) catches it.
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr on clean validation: %q", stderr.String())
	}
}

func TestValidateInvalidWorkflowExitsInvalidText(t *testing.T) {
	path := writeFixture(t, "wf.yaml", invalidDupID)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", path}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Errorf("Run = %d, want %d (stderr=%q)", code, ExitInvalid, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "AWF1004") {
		t.Errorf("expected AWF1004 on stdout; got %q", out)
	}
	if !strings.Contains(out, "1 error") {
		t.Errorf("expected '1 error' summary; got %q", out)
	}
	// Digest is still printed even when validation has errors — the digest is the canonical
	// hash, not a "valid" stamp. (See §E of the Phase 1 design.)
	if !strings.Contains(out, "digest: awf-d1:sha256:") {
		t.Errorf("expected digest line even on invalid workflow; got %q", out)
	}
}

func TestValidateCleanWorkflowJSONExitsOK(t *testing.T) {
	path := writeFixture(t, "wf.yaml", validMinimal)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--format", "json", path}, &stdout, &stderr)
	if code != ExitOK {
		t.Errorf("Run = %d, want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	var got struct {
		Path        string          `json:"path"`
		Digest      string          `json:"digest"`
		Diagnostics []ir.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v; stdout=%q", err, stdout.String())
	}
	if got.Path != path {
		t.Errorf("got.Path = %q, want %q", got.Path, path)
	}
	if !strings.HasPrefix(got.Digest, "awf-d1:sha256:") {
		t.Errorf("got.Digest = %q, want awf-d1:sha256: prefix", got.Digest)
	}
	if len(got.Diagnostics) != 0 {
		t.Errorf("got.Diagnostics = %v, want []", got.Diagnostics)
	}
	// Mirror of the text test: stderr must be silent so `awf validate --format json | jq` works.
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr on clean JSON validation: %q", stderr.String())
	}
}

func TestValidateInvalidWorkflowJSONExitsInvalid(t *testing.T) {
	path := writeFixture(t, "wf.yaml", invalidDupID)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--format", "json", path}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Errorf("Run = %d, want %d (stderr=%q)", code, ExitInvalid, stderr.String())
	}
	var got struct {
		Path        string          `json:"path"`
		Digest      string          `json:"digest"`
		Diagnostics []ir.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v; stdout=%q", err, stdout.String())
	}
	if len(got.Diagnostics) == 0 {
		t.Fatalf("expected at least one diagnostic; got none")
	}
	foundDup := false
	for _, d := range got.Diagnostics {
		if d.Code == "AWF1004" {
			foundDup = true
			break
		}
	}
	if !foundDup {
		t.Errorf("expected AWF1004 in diagnostics; got %+v", got.Diagnostics)
	}
}

// TestValidateExitCodesAreStable locks the three exit-code values so a future "let's renumber
// for symmetry with diff" idea is a loud test failure rather than a silent CI break in
// downstream tooling that grep's `awf validate; echo $?`.
func TestValidateExitCodesAreStable(t *testing.T) {
	if ExitOK != 0 || ExitInvalid != 1 || ExitUsage != 2 {
		t.Errorf("exit codes drifted: OK=%d Invalid=%d Usage=%d; want 0/1/2", ExitOK, ExitInvalid, ExitUsage)
	}
}
