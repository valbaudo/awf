package loader

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestLoadImportsWorkflowRelativeToDeclaringModule(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "modules", "fixtures"))
	if err := os.WriteFile(filepath.Join(dir, "modules", "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modules", "fixtures", "prompt.txt"), []byte("prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "modules", "recon.awf.yaml"), "workflow: recon\nversion: 1\nassets:\n  prompt: fixtures/prompt.txt\ncontainers:\n  lab:\n    compose: ./compose.yml\ngraph: []\n")
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  recon: modules/recon.awf.yaml\ncontainers: {}\ngraph: []\n")

	ld, err := Load(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root := ld.Root()
	if root == nil || root.ID != "" || root.Workflow != ld.Workflow || root.WorkflowPath != ld.WorkflowPath {
		t.Fatalf("root alias mismatch: root=%+v ld.WorkflowPath=%q", root, ld.WorkflowPath)
	}
	child, ok := ld.Module("recon")
	if !ok {
		t.Fatalf("missing recon module")
	}
	if child.Workflow.ID != "recon" {
		t.Fatalf("child workflow id = %q, want recon", child.Workflow.ID)
	}
	if _, ok := child.ComposeFiles["compose.yml"]; !ok {
		keys := mapKeys(child.ComposeFiles)
		slices.Sort(keys)
		t.Fatalf("child compose keys = %v, want compose.yml", keys)
	}
	asset := child.Assets["prompt"]
	if asset.DeclaredPath != "fixtures/prompt.txt" || len(asset.Files) != 1 || string(asset.Files[0].Bytes) != "prompt\n" {
		t.Fatalf("child asset = %+v", asset)
	}
	assertImportEdges(t, ld, []ir.LoadedImportEdge{{
		ParentID:     "",
		ImportID:     "recon",
		DeclaredPath: "modules/recon.awf.yaml",
		ChildID:      "recon",
	}})
}

func TestLoadImportRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  recon: ../recon.awf.yaml\ncontainers: {}\ngraph: []\n")

	_, err := Load(rootPath)
	assertLoadErrorCode(t, err, "AWF_IMPORT_PATH_ESCAPE")
}

func TestLoadImportMissingFileIsReadError(t *testing.T) {
	dir := t.TempDir()
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  recon: missing.awf.yaml\ncontainers: {}\ngraph: []\n")

	_, err := Load(rootPath)
	assertLoadErrorCode(t, err, "AWF_IMPORT_READ")
}

func TestLoadImportRejectsSymlinkComponent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "child.awf.yaml"), "workflow: child\nversion: 1\ncontainers: {}\ngraph: []\n")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatalf("os.Symlink failed: %v", err)
	}
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  recon: linked/child.awf.yaml\ncontainers: {}\ngraph: []\n")

	_, err := Load(rootPath)
	assertLoadErrorCode(t, err, "AWF_IMPORT_SYMLINK")
}

func TestLoadImportRejectsCycle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.awf.yaml"), "workflow: a\nversion: 1\nimports:\n  b: b.awf.yaml\ncontainers: {}\ngraph: []\n")
	writeFile(t, filepath.Join(dir, "b.awf.yaml"), "workflow: b\nversion: 1\nimports:\n  a: a.awf.yaml\ncontainers: {}\ngraph: []\n")

	_, err := Load(filepath.Join(dir, "a.awf.yaml"))
	assertLoadErrorCode(t, err, "AWF_IMPORT_CYCLE")
}

func TestLoadNestedImports(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "outer", "inner"))
	writeFile(t, filepath.Join(dir, "outer", "inner", "leaf.awf.yaml"), "workflow: leaf\nversion: 1\ncontainers: {}\ngraph: []\n")
	writeFile(t, filepath.Join(dir, "outer", "outer.awf.yaml"), "workflow: outer\nversion: 1\nimports:\n  inner: inner/leaf.awf.yaml\ncontainers: {}\ngraph: []\n")
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  outer: outer/outer.awf.yaml\ncontainers: {}\ngraph: []\n")

	ld, err := Load(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	if err := ld.WalkModules(func(m *ir.LoadedModule) error {
		gotIDs = append(gotIDs, m.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "outer", "outer.inner"}; !slices.Equal(gotIDs, want) {
		t.Fatalf("WalkModules ids = %v, want %v", gotIDs, want)
	}
	assertImportEdges(t, ld, []ir.LoadedImportEdge{
		{ParentID: "", ImportID: "outer", DeclaredPath: "outer/outer.awf.yaml", ChildID: "outer"},
		{ParentID: "outer", ImportID: "inner", DeclaredPath: "inner/leaf.awf.yaml", ChildID: "outer.inner"},
	})
}

func TestLoadSameFileImportedTwiceCreatesTwoLogicalModules(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "shared.awf.yaml"), "workflow: shared\nversion: 1\ncontainers: {}\ngraph: []\n")
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  first: shared.awf.yaml\n  second: ./shared.awf.yaml\ncontainers: {}\ngraph: []\n")

	ld, err := Load(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := ld.Module("first")
	if !ok {
		t.Fatal("missing first module")
	}
	second, ok := ld.Module("second")
	if !ok {
		t.Fatal("missing second module")
	}
	if first == second || first.ID != "first" || second.ID != "second" {
		t.Fatalf("logical modules not distinct: first=%p %+v second=%p %+v", first, first, second, second)
	}
	if first.WorkflowPath != second.WorkflowPath {
		t.Fatalf("same physical import paths differ: %q vs %q", first.WorkflowPath, second.WorkflowPath)
	}
	assertImportEdges(t, ld, []ir.LoadedImportEdge{
		{ParentID: "", ImportID: "first", DeclaredPath: "shared.awf.yaml", ChildID: "first"},
		{ParentID: "", ImportID: "second", DeclaredPath: "shared.awf.yaml", ChildID: "second"},
	})
}

func TestLoadImportRejectsInvalidImportID(t *testing.T) {
	for name, id := range map[string]string{
		"bad_char":  "bad.id",
		"bad_start": "1bad",
		"reserved":  "workflow",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "child.awf.yaml"), "workflow: child\nversion: 1\ncontainers: {}\ngraph: []\n")
			rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  "+quoteYAMLString(id)+": child.awf.yaml\ncontainers: {}\ngraph: []\n")

			_, err := Load(rootPath)
			assertLoadErrorCode(t, err, "AWF_IMPORT_ID_INVALID")
		})
	}
}

func assertLoadErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected LoadError code %s, got nil", want)
	}
	var loadErr *LoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("err = %T %v, want *LoadError", err, err)
	}
	if loadErr.Code != want {
		t.Fatalf("LoadError.Code = %s, want %s (err: %v)", loadErr.Code, want, err)
	}
}

func assertImportEdges(t *testing.T, ld *ir.LoadedDefinition, want []ir.LoadedImportEdge) {
	t.Helper()
	var got []ir.LoadedImportEdge
	if err := ld.WalkImportEdges(func(edge ir.LoadedImportEdge) error {
		got = append(got, edge)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("WalkImportEdges = %+v, want %+v", got, want)
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if strings.TrimSpace(body) == "" {
		t.Fatal("test helper requires non-empty body")
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
