package loader

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goyaml "github.com/goccy/go-yaml"

	"github.com/valbaudo/awf/ir"
)

func TestLoadValidFixture(t *testing.T) {
	ld, err := Load("testdata/valid/cve-pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ld.Workflow == nil || ld.Workflow.ID != "cve-pipeline" {
		t.Fatalf("Workflow = %#v", ld.Workflow)
	}
	if !filepath.IsAbs(ld.WorkflowPath) {
		t.Fatalf("WorkflowPath = %q (want absolute)", ld.WorkflowPath)
	}
	// Mixed shape: one image-backed container (no compose), one compose-backed.
	if len(ld.Workflow.Containers) != 2 {
		t.Fatalf("Containers count = %d, want 2", len(ld.Workflow.Containers))
	}
	if r, ok := ld.Workflow.Containers["runner"]; !ok || r.Image == "" || r.Compose != "" {
		t.Fatalf("expected image-backed `runner` with no compose; got %#v (all keys = %v)", r, keys(ld.Workflow.Containers))
	}
	if len(ld.ComposeFiles) != 1 {
		t.Fatalf("ComposeFiles count = %d, want 1 (only `lab` is compose-backed)", len(ld.ComposeFiles))
	}
	b, ok := ld.ComposeFiles["lab/compose.yml"]
	if !ok {
		t.Fatalf("ComposeFiles keys = %v, want forward-slash workflow-relative \"lab/compose.yml\"", keys(ld.ComposeFiles))
	}
	if !strings.Contains(string(b), "vulnerable:") {
		t.Fatalf("compose bytes look wrong: %q", b)
	}
	// Load normalizes Container.Compose to its cleaned forward-slash form so the IR field
	// and the ComposeFiles map key agree. Authored "./lab/compose.yml" must become "lab/compose.yml".
	c := ld.Workflow.Containers["lab"]
	if c.Compose != "lab/compose.yml" {
		t.Fatalf("Container.Compose = %q, want normalized %q", c.Compose, "lab/compose.yml")
	}
	// Graph must be decoded — the YAML→IR pipeline could silently lose it without an assertion.
	if len(ld.Workflow.Graph) != 1 {
		t.Fatalf("Graph len = %d, want 1", len(ld.Workflow.Graph))
	}
	cs, ok := ld.Workflow.Graph[0].(*ir.CodeStep)
	if !ok || cs.ID != "triage" || cs.Run != "./triage.sh" {
		t.Fatalf("Graph[0] = %#v, want *ir.CodeStep{ID: triage, Run: ./triage.sh}", ld.Workflow.Graph[0])
	}
}

func TestLoadMissingWorkflow(t *testing.T) {
	_, err := Load("testdata/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected error for missing workflow")
	}
}

func TestLoadBadYAML(t *testing.T) {
	_, err := Load("testdata/invalid/bad-yaml.yaml")
	if err == nil {
		t.Fatal("expected parse error")
	}
	// goccy emits *goyaml.SyntaxError; assert against its structured fields, not the rendered
	// message (which would false-positive on any incidental "5" in paths or hashes).
	var se *goyaml.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("expected *goyaml.SyntaxError unwrappable from %T: %v", err, err)
	}
	if se.Token == nil || se.Token.Position == nil {
		t.Fatalf("SyntaxError missing token/position: %#v", se)
	}
	if se.Token.Position.Line != 5 {
		t.Errorf("err on line %d, want 5; full err: %v", se.Token.Position.Line, err)
	}
}

func TestLoadMissingCompose(t *testing.T) {
	_, err := Load("testdata/invalid/no-such-compose.yaml")
	if err == nil {
		t.Fatal("expected error for missing compose file")
	}
	if !strings.Contains(err.Error(), "nope.yml") {
		t.Errorf("error should mention the missing path: %v", err)
	}
}

func TestLoadPathEscapeRejected(t *testing.T) {
	_, err := Load("testdata/invalid/path-escape.yaml")
	if err == nil {
		t.Fatal("expected error for `..` path escape")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error should say 'escape': %v", err)
	}
}

func TestLoadAbsoluteComposeRejected(t *testing.T) {
	_, err := Load("testdata/invalid/absolute-compose.yaml")
	if err == nil {
		t.Fatal("expected error for absolute compose path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should say 'absolute': %v", err)
	}
}

func TestLoadBackslashComposeRejected(t *testing.T) {
	// On darwin/linux filepath treats backslashes as ordinary characters, so a path like
	// `..\..\escape` would otherwise survive the prefix check and reach os.Root only to
	// fail with an opaque "no such file" error. composeRelPath rejects backslashes outright
	// for honest cross-OS attribution.
	dir := t.TempDir()
	wf := []byte("workflow: bs\nversion: 1\ncontainers:\n  lab:\n    compose: ..\\..\\escape\ngraph: []\n")
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, wf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for backslash in compose path")
	}
	if !strings.Contains(err.Error(), "backslash") {
		t.Errorf("error should say 'backslash': %v", err)
	}
}

func TestLoadSymlinkEscapeRejected(t *testing.T) {
	// Build an ephemeral fixture tree at runtime so we can introduce a symlink pointing OUT
	// of the workflow directory. os.Root must refuse to follow it. CI runs on linux and dev
	// runs on darwin; both support unprivileged symlink creation. If os.Symlink ever fails
	// here, that's a real signal — fail the test rather than silently skipping.
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.yml")
	if err := os.WriteFile(outside, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil.yml")); err != nil {
		t.Fatalf("os.Symlink failed (symlinks must work on supported platforms): %v", err)
	}
	wf := []byte("workflow: sym\nversion: 1\ncontainers:\n  lab:\n    compose: ./evil.yml\ngraph: []\n")
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, wf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for symlink escape; os.Root must refuse to follow it")
	}
}

func TestLoadInsideRootSymlinkAlsoRejected(t *testing.T) {
	// os.Root refuses ALL symlinks in the resolution path, not only escaping ones — even a
	// symlink to a sibling file inside the same rooted directory is refused. This locks that
	// conservative behavior so we notice if Go ever loosens it (it would be a real semantic
	// shift in our security posture).
	dir := t.TempDir()
	real := filepath.Join(dir, "real.yml")
	if err := os.WriteFile(real, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link.yml")); err != nil {
		t.Fatalf("os.Symlink failed: %v", err)
	}
	wf := []byte("workflow: inside\nversion: 1\ncontainers:\n  lab:\n    compose: ./link.yml\ngraph: []\n")
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, wf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected os.Root to refuse an inside-root symlink (conservative behavior locked here)")
	}
}

func keys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
