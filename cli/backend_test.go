package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// ---- newBackend tests (daemon-free) ----

func TestNewBackendFakeKindReturnsFake(t *testing.T) {
	t.Parallel()
	backend, cleanup, err := cli.NewBackendForTest(context.Background(), engine.BackendFake, "run-abc", "", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("newBackend(fake): %v", err)
	}
	defer cleanup()
	if backend == nil {
		t.Fatal("backend = nil")
	}
	if got := backend.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotNone (fake)", got)
	}
}

func TestNewBackendUnknownKindIsError(t *testing.T) {
	t.Parallel()
	_, _, err := cli.NewBackendForTest(context.Background(), "containerd", "run-abc", "", state.NewInMemoryBlobs())
	if err == nil {
		t.Fatal("err = nil, want non-nil (unknown kind)")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("err = %q, want to mention the unknown kind", err)
	}
}

// (No TestNewBackendEmptyKindIsError — the default arm of newBackend's
// switch handles "" with the same "unknown backend kind" error path; an
// explicit empty-case test would exercise dead code per slice-4.5 plan
// §Major #8.)

func TestNewBackendNativeKindReturnsNative(t *testing.T) {
	t.Parallel()
	workdirRoot := t.TempDir()
	backend, cleanup, err := cli.NewBackendForTest(context.Background(), engine.BackendNative, "run-native", workdirRoot, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("newBackend(native): %v", err)
	}
	defer cleanup()
	if backend == nil {
		t.Fatal("backend = nil")
	}
	// newBackend now wires blobs into the native backend (native.WithBlobs), so
	// snapshot capability is blob-contingent: with a non-nil Blobs it advertises
	// SnapshotFSArchive (the snapshot: workspace facility), not SnapshotNone.
	if got := backend.Capabilities().Snapshot; got != container.SnapshotFSArchive {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotFSArchive (native + blobs)", got)
	}
}

// ---- readBackendKindFromLog tests (pure function over event slice) ----

func TestReadBackendKindFromLogReturnsRecorded(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        engine.BackendFake,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if kind != engine.BackendFake {
		t.Errorf("kind = %q, want %q", kind, engine.BackendFake)
	}
}

func TestReadBackendKindFromLogDefaultsLegacyLogToDocker(t *testing.T) {
	t.Parallel()
	// Pre-slice-4.5 payload: no Backend field.
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if kind != engine.BackendDocker {
		t.Errorf("kind = %q, want %q (legacy log default)", kind, engine.BackendDocker)
	}
}

func TestReadBackendKindFromLogRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        "containerd",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	_, err = cli.ReadBackendKindFromLogForTest(events)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("err = %q, want to mention the unknown kind", err)
	}
}

func TestReadBackendKindFromLogErrorsIfNoRunStarted(t *testing.T) {
	t.Parallel()
	// Defensive: a log without a run.started event is malformed by
	// construction, but we want a clean error rather than an empty string
	// silently defaulting to docker.
	events := []state.Event{}
	_, err := cli.ReadBackendKindFromLogForTest(events)
	if err == nil {
		t.Fatal("err = nil, want non-nil (no run.started event)")
	}
	if !strings.Contains(err.Error(), "run.started") {
		t.Errorf("err = %q, want to mention 'run.started'", err)
	}
}

func TestReadBackendKindFromLogAdmitsNative(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        engine.BackendNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil (native resume is now admitted, mirror-Docker)", err)
	}
	if kind != engine.BackendNative {
		t.Errorf("kind = %q, want %q", kind, engine.BackendNative)
	}
}

func TestReadBackendKindFromLogRejectsAuto(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	_, err = cli.ReadBackendKindFromLogForTest(events)
	if err == nil {
		t.Fatal("err = nil, want non-nil (auto must not be recorded)")
	}
	if !strings.Contains(err.Error(), `unresolved backend "auto"`) {
		t.Errorf("err = %q, want unresolved auto diagnostic", err)
	}
}

func TestSelectRunBackendAutoDefaultsToNative(t *testing.T) {
	t.Parallel()
	got, err := cli.SelectRunBackendForTest("auto", simpleBackendWF())
	if err != nil {
		t.Fatalf("SelectRunBackend(auto): %v", err)
	}
	if got != engine.BackendNative {
		t.Errorf("selected backend = %q, want %q", got, engine.BackendNative)
	}
}

func TestSelectRunBackendAutoChoosesDockerForDockerOnlyFeatures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		wf   *ir.Workflow
	}{
		{
			name: "static image",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Image: "oci://example.com/lab@sha256:" + strings.Repeat("0", 64)},
			}},
		},
		{
			name: "static compose",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Compose: "lab/compose.yml", Service: "runner"},
			}},
		},
		{
			name: "workspace snapshot",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Snapshot: "workspace"},
			}},
		},
		{
			name: "runtime compose",
			wf: &ir.Workflow{
				Containers: map[string]ir.Container{},
				Graph: ir.NodeList{&ir.Compose{
					As: "lab", From: "step.compose.files.compose", Service: "runner",
					Body: ir.NodeList{},
				}},
			},
		},
		{
			name: "runtime map image",
			wf: &ir.Workflow{
				Containers: map[string]ir.Container{"lab": {}},
				Graph: ir.NodeList{&ir.Map{
					Container: "lab", Image: "{{ item.image }}", Body: ir.NodeList{},
				}},
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cli.SelectRunBackendForTest("auto", tc.wf)
			if err != nil {
				t.Fatalf("SelectRunBackend(auto): %v", err)
			}
			if got != engine.BackendDocker {
				t.Errorf("selected backend = %q, want %q", got, engine.BackendDocker)
			}
		})
	}
}

func TestSelectRunBackendAutoChoosesDockerForImportedFeature(t *testing.T) {
	t.Parallel()
	ld := &ir.LoadedDefinition{
		Workflow: simpleBackendWF(),
		Modules: map[string]*ir.LoadedModule{
			"": {ID: "", Workflow: simpleBackendWF()},
			"recon": {ID: "recon", Workflow: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Compose: "lab/compose.yml", Service: "runner"},
			}}},
		},
	}
	got, err := cli.SelectRunBackendForLoadedDefinitionForTest("auto", ld)
	if err != nil {
		t.Fatalf("SelectRunBackendForLoadedDefinition(auto): %v", err)
	}
	if got != engine.BackendDocker {
		t.Errorf("selected backend = %q, want %q", got, engine.BackendDocker)
	}
}

// TestSelectRunBackendExplicitNativeRejectsIncompatibleFeature checks that
// explicit --backend native still rejects the features native genuinely cannot
// run: static compose, runtime compose, and runtime map image. (Static image
// and snapshot: workspace are no longer rejected — see the Accepts tests below.)
func TestSelectRunBackendExplicitNativeRejectsIncompatibleFeature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		wf   *ir.Workflow
		path string
	}{
		{
			name: "static compose",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Compose: "lab/compose.yml", Service: "runner"},
			}},
			path: "containers.lab.compose",
		},
		{
			name: "runtime compose",
			wf: &ir.Workflow{
				Containers: map[string]ir.Container{},
				Graph: ir.NodeList{&ir.Compose{
					As: "lab", From: "step.compose.files.compose", Service: "runner",
					Body: ir.NodeList{},
				}},
			},
		},
		{
			name: "runtime map image",
			wf: &ir.Workflow{
				Containers: map[string]ir.Container{"lab": {}},
				Graph: ir.NodeList{&ir.Map{
					Container: "lab", Image: "{{ item.image }}", Body: ir.NodeList{},
				}},
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := cli.SelectRunBackendForTest(engine.BackendNative, tc.wf)
			if err == nil {
				t.Fatal("err = nil, want native rejection for native-incompatible feature")
			}
			if !strings.Contains(err.Error(), "--backend native") || !strings.Contains(err.Error(), "--backend docker") {
				t.Errorf("err = %q, want native rejection with docker guidance", err)
			}
			if tc.path != "" && !strings.Contains(err.Error(), tc.path) {
				t.Errorf("err = %q, want path %q", err, tc.path)
			}
		})
	}
}

// TestSelectRunBackendExplicitNativeAcceptsImageAndSnapshot checks the relaxed
// behavior: explicit --backend native now runs static-image and
// snapshot: workspace workflows (steps run on the host, image ignored).
func TestSelectRunBackendExplicitNativeAcceptsImageAndSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		wf   *ir.Workflow
	}{
		{
			name: "static image",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Image: "oci://example.com/lab@sha256:" + strings.Repeat("0", 64)},
			}},
		},
		{
			name: "snapshot workspace",
			wf: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {
					Image:    "oci://example.com/lab@sha256:" + strings.Repeat("0", 64),
					Snapshot: "workspace",
				},
			}},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cli.SelectRunBackendForTest(engine.BackendNative, tc.wf)
			if err != nil {
				t.Fatalf("SelectRunBackend(native): %v", err)
			}
			if got != engine.BackendNative {
				t.Errorf("selected backend = %q, want %q", got, engine.BackendNative)
			}
		})
	}
}

func TestSelectRunBackendExplicitNativeRejectsImportedIncompatibleFeature(t *testing.T) {
	t.Parallel()
	ld := &ir.LoadedDefinition{
		Workflow: simpleBackendWF(),
		Modules: map[string]*ir.LoadedModule{
			"": {ID: "", Workflow: simpleBackendWF()},
			"recon": {ID: "recon", Workflow: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Compose: "lab/compose.yml", Service: "runner"},
			}}},
		},
	}
	_, err := cli.SelectRunBackendForLoadedDefinitionForTest(engine.BackendNative, ld)
	if err == nil {
		t.Fatal("err = nil, want native rejection for imported native-incompatible feature")
	}
	if !strings.Contains(err.Error(), "module recon") || !strings.Contains(err.Error(), "containers.lab.compose") {
		t.Errorf("err = %q, want imported module/path diagnostic", err)
	}
}

// TestSelectRunBackendExplicitNativeAcceptsImportedImage checks that an imported
// module's static image no longer forces native rejection.
func TestSelectRunBackendExplicitNativeAcceptsImportedImage(t *testing.T) {
	t.Parallel()
	ld := &ir.LoadedDefinition{
		Workflow: simpleBackendWF(),
		Modules: map[string]*ir.LoadedModule{
			"": {ID: "", Workflow: simpleBackendWF()},
			"recon": {ID: "recon", Workflow: &ir.Workflow{Containers: map[string]ir.Container{
				"lab": {Image: "oci://example.com/lab@sha256:" + strings.Repeat("0", 64)},
			}}},
		},
	}
	got, err := cli.SelectRunBackendForLoadedDefinitionForTest(engine.BackendNative, ld)
	if err != nil {
		t.Fatalf("SelectRunBackendForLoadedDefinition(native): %v", err)
	}
	if got != engine.BackendNative {
		t.Errorf("selected backend = %q, want %q", got, engine.BackendNative)
	}
}

func TestSelectRunBackendExplicitFakeRemainsExplicit(t *testing.T) {
	t.Parallel()
	got, err := cli.SelectRunBackendForTest(engine.BackendFake, &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Image: "oci://example.com/lab@sha256:" + strings.Repeat("0", 64)},
		},
	})
	if err != nil {
		t.Fatalf("SelectRunBackend(fake): %v", err)
	}
	if got != engine.BackendFake {
		t.Errorf("selected backend = %q, want %q", got, engine.BackendFake)
	}
}

func simpleBackendWF() *ir.Workflow {
	return &ir.Workflow{Containers: map[string]ir.Container{}}
}

func TestSelectRunBackendRejectsUnknownKind(t *testing.T) {
	_, err := cli.SelectRunBackendForTest("containerd", simpleBackendWF())
	if err == nil {
		t.Fatal("unknown --backend kind accepted; want error")
	}
	if !strings.Contains(err.Error(), "containerd") || !strings.Contains(err.Error(), "invalid --backend value") {
		t.Fatalf("error %q must name the offending kind and be the invalid-value diagnostic", err)
	}
}
