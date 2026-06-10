package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

func testAssets(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("assets bucket records fake CopyTo calls; fake-only")
	}
	t.Run("stage_run_started_snapshot_bytes", func(t *testing.T) {
		testAssetsStageRunStartedSnapshotBytes(t, factory)
	})
	t.Run("resume_re_stages_run_started_snapshot", func(t *testing.T) {
		testAssetsResumeReStagesRunStartedSnapshot(t, factory)
	})
	t.Run("imported_child_assets_are_module_qualified", func(t *testing.T) {
		testAssetsImportedChildAssetsAreModuleQualified(t, factory)
	})
}

func testAssetsStageRunStartedSnapshotBytes(t *testing.T, factory BackendFactory) {
	t.Helper()

	var spy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			t.Fatalf("assets bucket factory returned %T, want *container.Fake", b)
		}
		fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
		spy = newAssetCopyToSpy(fake)
		return spy
	}, assetStageWorkflow)

	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "prompt.txt"), []byte("prompt snapshot\n"))
	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "fixtures", "a.txt"), []byte("alpha\n"))
	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "fixtures", "nested", "b.txt"), []byte("bravo\n"))

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	if spy == nil {
		t.Fatal("workflow did not create a fake backend")
	}

	started, err := engine.RunStartedDataFromEvents(mustFoldEvents(t, h))
	if err != nil {
		t.Fatalf("run.started: %v", err)
	}
	if got := len(started.Assets); got != 2 {
		t.Fatalf("run.started assets = %d, want 2", got)
	}
	if got := len(started.Assets["fixtures"].Files); got != 2 {
		t.Fatalf("fixtures file count = %d, want 2", got)
	}

	got := spy.stagedByPath()
	want := map[string]string{
		"/work/prompt.txt":            "prompt snapshot\n",
		"/work/fixtures/a.txt":        "alpha\n",
		"/work/fixtures/nested/b.txt": "bravo\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged files = %#v, want %#v", got, want)
	}
}

func testAssetsResumeReStagesRunStartedSnapshot(t *testing.T, factory BackendFactory) {
	t.Helper()

	var runSpy, resumeSpy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			t.Fatalf("assets bucket factory returned %T, want *container.Fake", b)
		}
		fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
		spy := newAssetCopyToSpy(fake)
		if runSpy == nil {
			fake.FailExecAfterN(0)
			runSpy = spy
		} else {
			resumeSpy = spy
		}
		return spy
	}, assetStageWorkflow)

	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "prompt.txt"), []byte("prompt snapshot\n"))
	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "fixtures", "a.txt"), []byte("alpha\n"))
	writeAssetFile(t, filepath.Join(h.baseDir, "assets", "fixtures", "nested", "b.txt"), []byte("bravo\n"))

	oc, _ := h.runWorkflow(t)
	if oc == "" {
		t.Fatal("first run produced no outcome (harness error before the workflow evaluated)")
	}
	if oc == engine.OutcomeOK {
		t.Fatal("first run unexpectedly ok; FailExecAfterN(0) should leave consume uncommitted")
	}
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold after failed run: %v", err)
	}
	if _, ok := rs.Completed["consume"]; ok {
		t.Fatal("consume committed in failed run; resume would skip the asset staging proof")
	}

	oc2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", oc2)
	}
	if resumeSpy == nil {
		t.Fatal("resume did not create a second fake backend")
	}

	want := map[string]string{
		"/work/prompt.txt":            "prompt snapshot\n",
		"/work/fixtures/a.txt":        "alpha\n",
		"/work/fixtures/nested/b.txt": "bravo\n",
	}
	if got := runSpy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first-run staged files = %#v, want %#v", got, want)
	}
	if got := resumeSpy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resume staged files = %#v, want %#v", got, want)
	}
}

func testAssetsImportedChildAssetsAreModuleQualified(t *testing.T, factory BackendFactory) {
	t.Helper()

	var spy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			t.Fatalf("assets bucket factory returned %T, want *container.Fake", b)
		}
		fake.ProgramExec("./consume-child.sh", container.ExecResult{ExitCode: 0}, nil)
		spy = newAssetCopyToSpy(fake)
		return spy
	}, assetImportedRootWorkflow)

	writeAssetFile(t, filepath.Join(h.baseDir, "child.awf.yaml"), []byte(assetImportedChildWorkflow))
	writeAssetFile(t, filepath.Join(h.baseDir, "child", "schema.json"), []byte("child schema\n"))

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	if spy == nil {
		t.Fatal("workflow did not create a fake backend")
	}

	started, err := engine.RunStartedDataFromEvents(mustFoldEvents(t, h))
	if err != nil {
		t.Fatalf("run.started: %v", err)
	}
	childAsset, ok := started.Assets["recon/schema"]
	if !ok {
		t.Fatalf("run.started assets missing recon/schema: %#v", started.Assets)
	}
	if childAsset.DeclaredPath != "child/schema.json" {
		t.Fatalf("recon/schema DeclaredPath = %q, want child/schema.json", childAsset.DeclaredPath)
	}

	got := spy.stagedByPath()
	want := map[string]string{
		"/work/schema.json": "child schema\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged files = %#v, want %#v", got, want)
	}
}

func writeAssetFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

var assetStageWorkflow = fmt.Sprintf(`workflow: conformance-assets
version: 1
assets:
  prompt: assets/prompt.txt
  fixtures: assets/fixtures
containers:
  lab:
    image: %s
graph:
  - id: consume
    container: lab
    run: "./consume.sh"
    retry: { attempts: 1 }
    input_files:
      /work/prompt.txt: asset.prompt
      /work/fixtures: asset.fixtures
`, fakeImageDigest)

var assetImportedRootWorkflow = `workflow: conformance-imported-assets-root
version: 1
imports:
  recon: child.awf.yaml
containers: {}
graph:
  - id: recon
    call: recon
`

var assetImportedChildWorkflow = fmt.Sprintf(`workflow: conformance-imported-assets-child
version: 1
assets:
  schema: child/schema.json
containers:
  lab:
    image: %s
graph:
  - id: consume
    container: lab
    run: "./consume-child.sh"
    retry: { attempts: 1 }
    input_files:
      /work/schema.json: asset.schema
`, fakeImageDigest)

type assetCopyToSpy struct {
	*container.Fake
	mu     sync.Mutex
	staged []container.InputFile
}

func newAssetCopyToSpy(fake *container.Fake) *assetCopyToSpy {
	return &assetCopyToSpy{Fake: fake}
}

func (s *assetCopyToSpy) CopyTo(ctx context.Context, h container.Handle, files []container.InputFile) error {
	if err := s.Fake.CopyTo(ctx, h, files); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		dup := make([]byte, len(f.Content))
		copy(dup, f.Content)
		s.staged = append(s.staged, container.InputFile{Path: f.Path, Content: dup})
	}
	return nil
}

func (s *assetCopyToSpy) stagedByPath() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.staged))
	for _, f := range s.staged {
		out[f.Path] = string(f.Content)
	}
	return out
}
