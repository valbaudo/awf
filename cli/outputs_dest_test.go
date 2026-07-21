package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// seedFilesAt seeds a run with one committed node at `node` whose output_files
// map (container path -> content) is written to the blob store and journaled on
// the node.completed event (NodeCompletedData.Files: "declared path -> CAS ref").
func seedFilesAt(t *testing.T, stateDir, runID, node string, files map[string]string) {
	t.Helper()
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	refs := make(map[string]string, len(files))
	for p, content := range files {
		ref, err := blobs.Put([]byte(content))
		if err != nil {
			t.Fatalf("Put %q: %v", p, err)
		}
		refs[p] = ref
	}
	writeRunLog(t, stateDir, runID,
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: runID, WorkflowDigest: "d"})},
		state.Event{Type: engine.EventNodeCompleted, Path: node, Data: marshal(t, engine.NodeCompletedData{Outcome: "ok", Files: refs})},
	)
}

// (a) happy path: a step's output_files materialize under --dest, mirroring the
// container path (strip leading slash), written by the host process.
func TestOutputsDestMaterializesFiles(t *testing.T) {
	stateDir := t.TempDir()
	seedFilesAt(t, stateDir, "r1", "report", map[string]string{
		"/out/report.json": `{"ok":true}`,
		"/out/sub/a.txt":   "hello",
	})
	dest := filepath.Join(t.TempDir(), "materialized")
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d want ExitOK; stderr=%s", rc, errb.String())
	}
	if got, err := os.ReadFile(filepath.Join(dest, "out", "report.json")); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("out/report.json = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "out", "sub", "a.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("out/sub/a.txt = %q, err=%v", got, err)
	}
}

// A relative container path (resolved against the workdir) mirrors under --dest
// too; an absolute path is NOT rejected — it is contained by stripping the slash.
func TestOutputsDestContainsAbsoluteAndRelative(t *testing.T) {
	stateDir := t.TempDir()
	seedFilesAt(t, stateDir, "r1", "report", map[string]string{
		"/etc/keep.txt": "absolute-is-fine", // contained at dest/etc/keep.txt, NOT the host /etc
		"rel/note.txt":  "relative-is-fine",
	})
	dest := t.TempDir()
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d want ExitOK; stderr=%s", rc, errb.String())
	}
	if got, err := os.ReadFile(filepath.Join(dest, "etc", "keep.txt")); err != nil || string(got) != "absolute-is-fine" {
		t.Fatalf("etc/keep.txt = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "rel", "note.txt")); err != nil || string(got) != "relative-is-fine" {
		t.Fatalf("rel/note.txt = %q, err=%v", got, err)
	}
}

// (c) collision: two files with the same basename in different dirs land at
// distinct destinations — the path is the key, so nothing overwrites.
func TestOutputsDestNoCollisionOnSameBasename(t *testing.T) {
	stateDir := t.TempDir()
	seedFilesAt(t, stateDir, "r1", "report", map[string]string{
		"/a/report.json": "A",
		"/b/report.json": "B",
	})
	dest := t.TempDir()
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d; stderr=%s", rc, errb.String())
	}
	a, _ := os.ReadFile(filepath.Join(dest, "a", "report.json"))
	b, _ := os.ReadFile(filepath.Join(dest, "b", "report.json"))
	if string(a) != "A" || string(b) != "B" {
		t.Fatalf("collision: a=%q b=%q", a, b)
	}
}

// (b) SECURITY: a `..` traversal path is rejected and nothing is written outside
// --dest. This is the new host-writing trust boundary (container-produced names).
func TestOutputsDestRejectsDotDotTraversal(t *testing.T) {
	for _, evil := range []string{"../pwned", "../../pwned", "/../pwned", "a/../../pwned"} {
		stateDir := t.TempDir()
		seedFilesAt(t, stateDir, "r1", "report", map[string]string{evil: "PWNED"})
		destParent := t.TempDir()
		dest := filepath.Join(destParent, "d")
		var out, errb bytes.Buffer
		rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb)
		if rc == ExitOK {
			t.Fatalf("evil=%q: rc=ExitOK, want failure", evil)
		}
		if _, err := os.Stat(filepath.Join(destParent, "pwned")); !os.IsNotExist(err) {
			t.Fatalf("evil=%q wrote outside dest (destParent/pwned exists): %v", evil, err)
		}
	}
}

// (b) SECURITY: a symlink inside --dest pointing outside must not be traversed —
// os.Root refuses, and nothing is written through the link.
func TestOutputsDestRejectsSymlinkEscape(t *testing.T) {
	stateDir := t.TempDir()
	outside := t.TempDir()
	seedFilesAt(t, stateDir, "r1", "report", map[string]string{"link/pwned": "PWNED"})
	dest := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	var out, errb bytes.Buffer
	rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb)
	if rc == ExitOK {
		t.Fatalf("symlink escape not rejected (rc=ExitOK)")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("wrote through symlink outside dest: %v", err)
	}
}

// (d) empty: a step that produced no output_files exits cleanly and writes nothing.
func TestOutputsDestEmptyFilesExitsClean(t *testing.T) {
	stateDir := t.TempDir()
	seedNodeAt(t, stateDir, "r1", "report", `{"x":1}`) // has OutputsRef, no Files
	dest := filepath.Join(t.TempDir(), "d")
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d want ExitOK; stderr=%s", rc, errb.String())
	}
}

// --dest without --step is a usage error (it materializes a specific step's files).
func TestOutputsDestRequiresStep(t *testing.T) {
	stateDir := t.TempDir()
	seedNodeAt(t, stateDir, "r1", "report", `{}`)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--dest", t.TempDir(), "--state-dir", stateDir}, &out, &errb); rc != ExitUsage {
		t.Fatalf("rc=%d want ExitUsage", rc)
	}
}

// REGRESSION (adversarial review F5): two DISTINCT container paths that differ
// only by a leading separator both strip to the same relative destination, so
// one silently overwrote the other. A relative output_files path resolves
// against the container workdir, so "/out/x" and "out/x" are genuinely
// different files — materializing them to one host path is data loss.
// Materialization must refuse the ambiguity loudly instead.
func TestOutputsDestRejectsDuplicateDestinations(t *testing.T) {
	stateDir := t.TempDir()
	seedFilesAt(t, stateDir, "r1", "report", map[string]string{
		"/out/x": "ABSOLUTE",
		"out/x":  "RELATIVE",
	})
	dest := t.TempDir()
	var out, errb bytes.Buffer
	rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb)
	if rc == ExitOK {
		t.Fatalf("rc=ExitOK: two paths collapsing to one destination must fail loudly; stderr=%s", errb.String())
	}
	if got, err := os.ReadFile(filepath.Join(dest, "out", "x")); err == nil {
		t.Fatalf("wrote a colliding destination anyway (content=%q); nothing should be materialized", got)
	}
}

// An unusable declared path must be refused, not turned into a bogus write.
func TestOutputsDestRejectsUnusablePaths(t *testing.T) {
	for _, bad := range []string{"/", "", ".", "/out/x/"} {
		stateDir := t.TempDir()
		seedFilesAt(t, stateDir, "r1", "report", map[string]string{bad: "X"})
		dest := t.TempDir()
		var out, errb bytes.Buffer
		if rc := cliOutputs([]string{"r1", "--step", "report", "--dest", dest, "--state-dir", stateDir}, &out, &errb); rc == ExitOK {
			t.Errorf("path %q: rc=ExitOK, want failure", bad)
		}
	}
}
