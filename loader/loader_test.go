package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
		t.Fatalf("expected image-backed `runner` with no compose; got %#v (all keys = %v)", r, slices.Sorted(maps.Keys(ld.Workflow.Containers)))
	}
	if len(ld.ComposeFiles) != 1 {
		t.Fatalf("ComposeFiles count = %d, want 1 (only `lab` is compose-backed)", len(ld.ComposeFiles))
	}
	b, ok := ld.ComposeFiles["lab/compose.yml"]
	if !ok {
		t.Fatalf("ComposeFiles keys = %v, want forward-slash workflow-relative \"lab/compose.yml\"", slices.Sorted(maps.Keys(ld.ComposeFiles)))
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
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping fs.ErrNotExist", err)
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
	// of the workflow directory. AWF rejects it before opening. CI runs on linux and dev runs
	// on darwin; both support unprivileged symlink creation. If os.Symlink ever fails here,
	// that's a real signal — fail the test rather than silently skipping.
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
		t.Fatal("expected error for symlink escape")
	}
}

func TestLoadDeduplicatesSharedComposePath(t *testing.T) {
	// Two containers referencing the same cleaned compose path (different surface forms
	// "./compose.yml" and "compose.yml") must result in ONE read and ONE entry in
	// ComposeFiles. Reading twice would open a TOCTOU window where the bytes could differ
	// between reads, destabilizing the future spec §E compose-fold digest.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := []byte("workflow: dedup\nversion: 1\ncontainers:\n  a:\n    compose: ./compose.yml\n  b:\n    compose: compose.yml\ngraph: []\n")
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, wf, 0o644); err != nil {
		t.Fatal(err)
	}
	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ld.ComposeFiles) != 1 {
		t.Fatalf("ComposeFiles count = %d, want 1 (deduped)", len(ld.ComposeFiles))
	}
	if a := ld.Workflow.Containers["a"].Compose; a != "compose.yml" {
		t.Errorf("Containers[a].Compose = %q, want normalized %q", a, "compose.yml")
	}
	if b := ld.Workflow.Containers["b"].Compose; b != "compose.yml" {
		t.Errorf("Containers[b].Compose = %q, want normalized %q", b, "compose.yml")
	}
}

func TestLoadInsideRootSymlinkAlsoRejected(t *testing.T) {
	// os.Root confines opens to the workflow directory, but it follows symlinks that remain
	// inside that root. AWF rejects symlinks explicitly, including a symlink to a sibling file
	// inside the same rooted directory.
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.yml")
	if err := os.WriteFile(realPath, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, filepath.Join(dir, "link.yml")); err != nil {
		t.Fatalf("os.Symlink failed: %v", err)
	}
	wf := []byte("workflow: inside\nversion: 1\ncontainers:\n  lab:\n    compose: ./link.yml\ngraph: []\n")
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, wf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected AWF to refuse an inside-root symlink")
	}
}

func TestLoadAssetFileSnapshot(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello asset\n")
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: asset-file\nversion: 1\nassets:\n  prompt: prompt.txt\ncontainers: {}\ngraph: []\n")
	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := ld.Assets["prompt"]
	if asset.ID != "prompt" || asset.DeclaredPath != "prompt.txt" || asset.IsDir {
		t.Fatalf("asset metadata = %+v", asset)
	}
	if len(asset.Files) != 1 {
		t.Fatalf("asset file count = %d, want 1", len(asset.Files))
	}
	f := asset.Files[0]
	if f.Path != "." || string(f.Bytes) != string(content) || f.Size() != int64(len(content)) {
		t.Fatalf("asset file = %+v", f)
	}
	sum := sha256.Sum256(content)
	if f.SHA256() != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256() = %q, want %x", f.SHA256(), sum)
	}
}

func TestLoadAssetDirectorySnapshotSorted(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "fixtures", "nested"))
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "z.txt"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "nested", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: asset-dir\nversion: 1\nassets:\n  fixtures: fixtures\ncontainers: {}\ngraph: []\n")
	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := ld.Assets["fixtures"]
	if !asset.IsDir {
		t.Fatalf("asset IsDir = false, want true: %+v", asset)
	}
	got := []string{}
	for _, f := range asset.Files {
		got = append(got, f.Path)
	}
	want := []string{"nested/a.txt", "z.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("asset file paths = %v, want %v", got, want)
	}
}

func TestLoadAssetNormalizesWorkflowAssetPathForDigest(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "fixtures"))
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: asset-normalize\nversion: 1\nassets:\n  fixtures: ./fixtures\ncontainers: {}\ngraph: []\n")
	withDot, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := withDot.Workflow.Assets["fixtures"]; got != "fixtures" {
		t.Fatalf("normalized Workflow.Assets[fixtures] = %q, want fixtures", got)
	}
	dWithDot, err := withDot.Workflow.ComputeDigest(withDot.ComposeFiles, withDot.Assets)
	if err != nil {
		t.Fatal(err)
	}

	wfPath = writeWorkflow(t, dir, "workflow: asset-normalize\nversion: 1\nassets:\n  fixtures: fixtures\ncontainers: {}\ngraph: []\n")
	withoutDot, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	dWithoutDot, err := withoutDot.Workflow.ComputeDigest(withoutDot.ComposeFiles, withoutDot.Assets)
	if err != nil {
		t.Fatal(err)
	}
	if dWithDot != dWithoutDot {
		t.Fatalf("digest drifted on equivalent asset path spelling: %s vs %s", dWithDot, dWithoutDot)
	}
}

func TestLoadAssetRejectsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "empty"))
	wfPath := writeWorkflow(t, dir, "workflow: empty-asset\nversion: 1\nassets:\n  empty: empty\ncontainers: {}\ngraph: []\n")
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for empty asset directory")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty asset id/path: %v", err)
	}
}

func TestLoadAssetRejectsEmptyDeclaredPath(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflow(t, dir, "workflow: empty-asset-path\nversion: 1\nassets:\n  input: \"\"\ncontainers: {}\ngraph: []\n")
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for empty asset path")
	}
	if !strings.Contains(err.Error(), "input") || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention asset id and empty path: %v", err)
	}
}

func TestLoadAssetRejectsUnsafeDeclaredPaths(t *testing.T) {
	for name, declared := range map[string]string{
		"absolute":  filepath.Join(string(filepath.Separator), "tmp", "asset.txt"),
		"escape":    "../asset.txt",
		"backslash": `dir\asset.txt`,
		"nul":       "bad\x00path",
		"tab":       "bad\tpath",
		"carriage":  "bad\rpath",
		"linefeed":  "bad\npath",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			wfPath := writeWorkflow(t, dir, "workflow: unsafe-asset\nversion: 1\nassets:\n  input: "+quoteYAMLString(declared)+"\ncontainers: {}\ngraph: []\n")
			_, err := Load(wfPath)
			if err == nil {
				t.Fatalf("expected error for declared path %q", declared)
			}
			if !strings.Contains(err.Error(), "input") || !strings.Contains(err.Error(), strconv.Quote(declared)) {
				t.Fatalf("error should mention asset id and path; got %v", err)
			}
		})
	}
}

func TestLoadAssetRejectsSymlinkAtDeclaredPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("os.Symlink failed: %v", err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: symlink-asset\nversion: 1\nassets:\n  input: link.txt\ncontainers: {}\ngraph: []\n")
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for symlink asset")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should mention symlink: %v", err)
	}
}

func TestLoadAssetRejectsSymlinkInsideDirectory(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "fixtures"))
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "fixtures", "link.txt")); err != nil {
		t.Fatalf("os.Symlink failed: %v", err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: symlink-dir-asset\nversion: 1\nassets:\n  fixtures: fixtures\ncontainers: {}\ngraph: []\n")
	_, err := Load(wfPath)
	if err == nil {
		t.Fatal("expected error for symlink inside asset directory")
	}
	if !strings.Contains(err.Error(), "fixtures") || !strings.Contains(err.Error(), "link.txt") {
		t.Fatalf("error should mention asset id/path and symlink path: %v", err)
	}
}

func TestLoadAssetRejectsUnsafeDirectoryEntryPath(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "fixtures"))
	for _, name := range []string{"bad\tname.txt", `bad\name.txt`} {
		if err := os.WriteFile(filepath.Join(dir, "fixtures", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		wfPath := writeWorkflow(t, dir, "workflow: unsafe-entry\nversion: 1\nassets:\n  fixtures: fixtures\ncontainers: {}\ngraph: []\n")
		_, err := Load(wfPath)
		if err == nil {
			t.Fatalf("expected error for unsafe asset entry path %q", name)
		}
		if !strings.Contains(err.Error(), "fixtures") || !strings.Contains(err.Error(), "not permitted") {
			t.Fatalf("error should mention asset id and path rejection: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "fixtures", name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadAssetsRejectsWorkflowWideFileCountLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	_, err = loadAssetsWithLimits(root, map[string]string{
		"a": "a.txt",
		"b": "b.txt",
		"c": "c.txt",
	}, assetLimits{fileBytes: MaxAssetFileBytes, totalBytes: MaxAssetTotalBytes, files: 2})
	if err == nil {
		t.Fatal("expected workflow-wide asset file count limit error")
	}
	if !strings.Contains(err.Error(), "file count") {
		t.Fatalf("error should mention file count limit: %v", err)
	}
}

func TestLoadAssetsRejectsWorkflowWideTotalBytesLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	_, err = loadAssetsWithLimits(root, map[string]string{
		"a": "a.txt",
		"b": "b.txt",
	}, assetLimits{fileBytes: MaxAssetFileBytes, totalBytes: 3, files: MaxAssetFiles})
	if err == nil {
		t.Fatal("expected workflow-wide asset total byte limit error")
	}
	if !strings.Contains(err.Error(), "total bytes") {
		t.Fatalf("error should mention total bytes limit: %v", err)
	}
}

func TestLoadAssetRejectsSingleFileOverLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxAssetFileBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflow(t, dir, "workflow: huge-asset\nversion: 1\nassets:\n  huge: huge.bin\ncontainers: {}\ngraph: []\n")
	_, err = Load(wfPath)
	if err == nil {
		t.Fatal("expected error for asset file over MaxAssetFileBytes")
	}
	if !strings.Contains(err.Error(), "huge") || !strings.Contains(err.Error(), `"."`) {
		t.Fatalf("error should mention asset id and path: %v", err)
	}
}

func TestLoadAssetDirectoryFilesContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("hello asset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "fixtures")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"a.txt": "alpha", "b.txt": "bravo bravo"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wfPath := writeWorkflow(t, dir, "workflow: asset-inv\nversion: 1\nassets:\n  prompt: prompt.txt\n  fixtures: fixtures\ncontainers: {}\ngraph: []\n")
	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	files := ld.Assets["fixtures"].Files
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) != 2 || string(files[0].Bytes) != "alpha" || string(files[1].Bytes) != "bravo bravo" {
		t.Fatalf("fixtures asset = %+v, want two files sorted by path: alpha, bravo bravo", files)
	}
}

func writeWorkflow(t *testing.T, dir, body string) string {
	t.Helper()
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return wfPath
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func quoteYAMLString(s string) string {
	repl := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\x00", `\0`, "\t", `\t`, "\r", `\r`, "\n", `\n`)
	return `"` + repl.Replace(s) + `"`
}
