package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/container"
)

const inputSchemaWF = `
workflow: with-input
version: 1
input:
  type: object
  additionalProperties: false
  required: [cve_id]
  properties:
    cve_id: { type: string }
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: echo_cve
    container: lab
    run: "echo {{ input.cve_id }}"
`

func writeInputSchemaWF(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(p, []byte(inputSchemaWF), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunInputFileReadsAndValidates: --input-file reads JSON from a file, validates
// it against workflow.input, and binds it identically to inline --input (the step's
// templated command proves the value flowed through).
func TestRunInputFileReadsAndValidates(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo CVE-2024-9999", container.ExecResult{ExitCode: 0, Stdout: []byte("ok\n")}, nil)
	wfPath := writeInputSchemaWF(t)
	inPath := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(inPath, []byte(`{"cve_id":"CVE-2024-9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", t.TempDir(), "--input-file", inPath, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "echo CVE-2024-9999" {
		t.Errorf("step did not bind file input; calls = %+v", fake.Calls)
	}
}

// TestRunInputFileFromStdin: `--input-file -` reads run input from stdin (injected
// via Runner.Stdin) and validates it the same way.
func TestRunInputFileFromStdin(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo CVE-2024-1111", container.ExecResult{ExitCode: 0, Stdout: []byte("ok\n")}, nil)
	wfPath := writeInputSchemaWF(t)
	runner := newTestRunner(t, fake)
	runner.Stdin = strings.NewReader(`{"cve_id":"CVE-2024-1111"}`)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", t.TempDir(), "--input-file", "-", wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "echo CVE-2024-1111" {
		t.Errorf("step did not bind stdin input; calls = %+v", fake.Calls)
	}
}

// TestRunInputAndInputFileMutuallyExclusive: supplying both --input and --input-file
// is a usage error (exit 2).
func TestRunInputAndInputFileMutuallyExclusive(t *testing.T) {
	t.Parallel()
	wfPath := writeInputSchemaWF(t)
	inPath := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(inPath, []byte(`{"cve_id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", t.TempDir(), "--input", `{"cve_id":"y"}`, "--input-file", inPath, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage (mutually exclusive); stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr lacks mutual-exclusion message: %s", stderr.String())
	}
}

// TestRunInputFileMissingFileErrors: an unreadable --input-file path is a usage error.
func TestRunInputFileMissingFileErrors(t *testing.T) {
	t.Parallel()
	wfPath := writeInputSchemaWF(t)
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", t.TempDir(), "--input-file", filepath.Join(t.TempDir(), "nope.json"), wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage (unreadable input file); stderr: %s", rc, stderr.String())
	}
}
