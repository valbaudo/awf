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

// writeTwoInputFilesWorkflow declares two top-level input files (document, image)
// with an empty graph, for the repeatable --input-files end-to-end tests.
func writeTwoInputFilesWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "two-input-files.yaml")
	if err := os.WriteFile(path, []byte(`workflow: two-input-files
version: 1
input_files:
  document: {}
  image: {}
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCLIRunInputFilesRepeatedFormBindsBoth (S8): two --input-files flags bind both
// declared names — the new repeatable form.
func TestCLIRunInputFilesRepeatedFormBindsBoth(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeTwoInputFilesWorkflow(t, wfDir)
	docPath := filepath.Join(wfDir, "doc.txt")
	imgPath := filepath.Join(wfDir, "img.png")
	if err := os.WriteFile(docPath, []byte("doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "document=" + docPath,
		"--input-files", "image=" + imgPath,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	started := readRunStartedData(t, stateDir, "test-run-1")
	for _, name := range []string{"document", "image"} {
		if ref, ok := started.InputFiles[name]; !ok || ref == "" {
			t.Errorf("run.started.InputFiles missing %q; got %+v", name, started.InputFiles)
		}
	}
}

// TestCLIRunInputFilesCommaInPathViaRepeatedForm (S8): a real path containing a
// comma binds correctly when supplied via the repeated form (no comma-splitting).
func TestCLIRunInputFilesCommaInPathViaRepeatedForm(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeTwoInputFilesWorkflow(t, wfDir)
	commaPath := filepath.Join(wfDir, "a,b.txt") // filename with a comma
	plainPath := filepath.Join(wfDir, "c.png")
	if err := os.WriteFile(commaPath, []byte("comma"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "document=" + commaPath, // comma path, safe via repeated form
		"--input-files", "image=" + plainPath,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	started := readRunStartedData(t, stateDir, "test-run-1")
	if ref, ok := started.InputFiles["document"]; !ok || ref == "" {
		t.Errorf("comma-path input not bound; got %+v", started.InputFiles)
	}
}

// TestCLIRunInputFilesLegacyCSVStillBindsBoth (S8): the legacy single comma-
// separated value still binds multiple names.
func TestCLIRunInputFilesLegacyCSVStillBindsBoth(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfDir := t.TempDir()
	wfPath := writeTwoInputFilesWorkflow(t, wfDir)
	docPath := filepath.Join(wfDir, "doc.txt")
	imgPath := filepath.Join(wfDir, "img.png")
	if err := os.WriteFile(docPath, []byte("doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--backend", "fake",
		"--input-files", "document=" + docPath + ",image=" + imgPath, // single legacy CSV value
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	started := readRunStartedData(t, stateDir, "test-run-1")
	for _, name := range []string{"document", "image"} {
		if ref, ok := started.InputFiles[name]; !ok || ref == "" {
			t.Errorf("legacy CSV did not bind %q; got %+v", name, started.InputFiles)
		}
	}
}
