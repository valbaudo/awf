package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

// writeInputFilesWorkflow writes a workflow declaring one top-level input file
// named "document" and an empty graph. The empty graph keeps the happy-path
// test focused on the run-start supply channel (run.started.InputFiles) without
// requiring a live agent/provider to consume the file.
func writeInputFilesWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "with-input-files.yaml")
	if err := os.WriteFile(path, []byte(`workflow: with-input-files
version: 1
input_files:
  document: {}
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRunInputFilesRecordedInRunStartedAndRoundTrips(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeInputFilesWorkflow(t, wfDir)

	docBytes := []byte("hello, this is the supplied document\n")
	docPath := filepath.Join(wfDir, "doc.txt")
	if err := os.WriteFile(docPath, docBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "document=" + docPath,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}

	// run.started records the input-file manifest: name -> non-empty CAS ref.
	started := readRunStartedData(t, stateDir, "test-run-1")
	ref, ok := started.InputFiles["document"]
	if !ok {
		t.Fatalf("run.started.InputFiles missing %q; got %+v", "document", started.InputFiles)
	}
	if ref == "" {
		t.Fatalf("run.started.InputFiles[document] ref is empty")
	}

	// The blob content round-trips the supplied bytes.
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	got, err := blobs.Get(ref)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", ref, err)
	}
	if !bytes.Equal(got, docBytes) {
		t.Fatalf("blob content = %q, want %q", got, docBytes)
	}
}

func TestCLIRunInputFilesUnknownNameIsExitUsage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeInputFilesWorkflow(t, wfDir)
	docPath := filepath.Join(wfDir, "doc.txt")
	if err := os.WriteFile(docPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "unknown=" + docPath,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown") || !strings.Contains(stderr.String(), "document") {
		t.Errorf("stderr should name the unknown supplied name and the declared name(s); got %q", stderr.String())
	}
	// Rejected pre-flight: no orphan log.
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log exists; err = %v, want fs.ErrNotExist", err)
	}
}

func TestCLIRunInputFilesMissingDeclaredIsExitUsage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeInputFilesWorkflow(t, wfDir)

	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		// No --input-files at all: the declared "document" is unsupplied.
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "document") {
		t.Errorf("stderr should name the missing declared input file; got %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log exists; err = %v, want fs.ErrNotExist", err)
	}
}

func TestCLIRunInputFilesNonexistentPathIsExitUsage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeInputFilesWorkflow(t, wfDir)

	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "document=" + filepath.Join(wfDir, "no-such-file.txt"),
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log exists; err = %v, want fs.ErrNotExist", err)
	}
}

func TestCLIRunInputFilesMalformedEntryIsExitUsage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeInputFilesWorkflow(t, wfDir)

	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		// No '=' separator: malformed entry.
		"--input-files", "document",
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log exists; err = %v, want fs.ErrNotExist", err)
	}
}
