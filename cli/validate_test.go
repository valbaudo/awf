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
	code := Run([]string{"validate", "--output", "xml", path}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("Run(validate --output xml) = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown --output") {
		t.Errorf("expected 'unknown --output' on stderr; got %q", stderr.String())
	}
}

func TestValidateLoadErrorExitsInvalidText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := "/no/such/file.yaml"
	code := Run([]string{"validate", path}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Errorf("Run(validate /no/such/file) = %d, want %d", code, ExitInvalid)
	}
	if stderr.Len() > 0 {
		t.Errorf("expected loader diagnostic on stdout, got stderr %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "AWF_IMPORT_READ") {
		t.Errorf("expected load error code on stdout; got %q", out)
	}
	if !strings.Contains(out, "at "+path+": open workflow directory") {
		t.Errorf("expected source attribution in stdout; got %q", out)
	}
}

func TestValidateLoadErrorJSONDiagnostic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := "/no/such/file.yaml"
	code := Run([]string{"validate", "--output", "json", path}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Errorf("Run(validate --output json /no/such/file) = %d, want %d", code, ExitInvalid)
	}
	if stderr.Len() > 0 {
		t.Errorf("expected loader diagnostic JSON on stdout, got stderr %q", stderr.String())
	}
	var got struct {
		Path        string          `json:"path"`
		Diagnostics []ir.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v; stdout=%q", err, stdout.String())
	}
	if got.Path != path {
		t.Errorf("Path = %q, want %q", got.Path, path)
	}
	if len(got.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want 1 diagnostic", got.Diagnostics)
	}
	d := got.Diagnostics[0]
	if d.Code != "AWF_IMPORT_READ" || d.Source != path {
		t.Errorf("diagnostic = %+v, want AWF_IMPORT_READ with Source", d)
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
	code := Run([]string{"validate", "--output", "json", path}, &stdout, &stderr)
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
	// Wire-format contract: the JSON output must contain the literal empty-array token, not
	// "null". encoding/json marshals a nil []ir.Diagnostic as null; the normalization in
	// cliValidate must prevent that.
	if !strings.Contains(stdout.String(), `"diagnostics": []`) {
		t.Errorf("expected '\"diagnostics\": []' (empty array) in JSON output; got %s", stdout.String())
	}
	// Mirror of the text test: stderr must be silent so `awf validate --output json | jq` works.
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr on clean JSON validation: %q", stderr.String())
	}
}

func TestValidateInvalidWorkflowJSONExitsInvalid(t *testing.T) {
	path := writeFixture(t, "wf.yaml", invalidDupID)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--output", "json", path}, &stdout, &stderr)
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

// TestValidateExitCodesAreStable locks the exit-code values so a future "let's renumber
// for symmetry with diff" idea is a loud test failure rather than a silent CI break in
// downstream tooling that grep's `awf validate; echo $?`.
// Locked by tests: ExitOK=0, ExitInvalid=1, ExitUsage=2, ExitRunFailed=1, ExitInfra=3.
func TestValidateExitCodesAreStable(t *testing.T) {
	if ExitOK != 0 || ExitInvalid != 1 || ExitUsage != 2 {
		t.Errorf("exit codes drifted: OK=%d Invalid=%d Usage=%d; want 0/1/2", ExitOK, ExitInvalid, ExitUsage)
	}
	if ExitRunFailed != 1 {
		t.Errorf("ExitRunFailed = %d, want 1", ExitRunFailed)
	}
	if ExitInfra != 3 {
		t.Errorf("ExitInfra = %d, want 3", ExitInfra)
	}
}

func TestPrintTextResultSummaryBranches(t *testing.T) {
	// printTextResult formats the summary line in four shapes (clean / errors only / warnings
	// only / both); existing tests only exercise "ok" (clean) and "1 error". This test calls
	// the renderer directly with synthesized diagnostics to cover the remaining three
	// branches plus the plural(n>1) path that the rest of the suite never hits.
	cases := []struct {
		name        string
		diags       []ir.Diagnostic
		wantSummary string // substring that must appear in the first line
	}{
		{
			name:        "errors-plural",
			diags:       []ir.Diagnostic{{Severity: ir.Error, Code: "AWF1004", Message: "x"}, {Severity: ir.Error, Code: "AWF1004", Message: "y"}},
			wantSummary: "2 errors",
		},
		{
			name:        "warnings-only-singular",
			diags:       []ir.Diagnostic{{Severity: ir.Warning, Code: "AWF2002", Message: "w"}},
			wantSummary: "1 warning",
		},
		{
			name:        "warnings-only-plural",
			diags:       []ir.Diagnostic{{Severity: ir.Warning, Code: "AWF2002", Message: "w1"}, {Severity: ir.Warning, Code: "AWF2002", Message: "w2"}},
			wantSummary: "2 warnings",
		},
		{
			name:        "errors-and-warnings",
			diags:       []ir.Diagnostic{{Severity: ir.Error, Code: "AWF1004", Message: "e"}, {Severity: ir.Warning, Code: "AWF2002", Message: "w"}},
			wantSummary: "1 error, 1 warning",
		},
		{
			name:        "errors-plural-warnings-plural",
			diags:       []ir.Diagnostic{{Severity: ir.Error}, {Severity: ir.Error}, {Severity: ir.Warning}, {Severity: ir.Warning}, {Severity: ir.Warning}},
			wantSummary: "2 errors, 3 warnings",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printTextResult(&buf, "wf.yaml", "", tc.diags) // empty digest so we don't assert on it
			out := buf.String()
			// First line is the summary; check substring presence (the line also has the path prefix).
			firstLine := strings.SplitN(out, "\n", 2)[0]
			if !strings.Contains(firstLine, tc.wantSummary) {
				t.Errorf("summary line = %q, want substring %q (full output: %q)", firstLine, tc.wantSummary, out)
			}
		})
	}
}
