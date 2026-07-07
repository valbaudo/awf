package cli

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// F5 (U4): explicit --backend native must fail closed on cmd:/keepalive: —
// neither has a host equivalent (a cmd: service is a keepalive sidecar;
// keepalive: false asks native to not keep a container alive, but native has
// no container to begin with). firstNativeIncompatibleFeature is the pure
// detector selectRunBackend consults under explicit native; these tests
// exercise it directly. (firstNativeIncompatibleFeature does not validate the
// image reference itself, so a placeholder digest string is fine here.)

func TestNativeIncompatible_CmdRejected(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Cmd: []string{"sleep", "infinity"}},
	}}
	f, ok := firstNativeIncompatibleFeature(wf)
	if !ok || f.Kind != "cmd" {
		t.Fatalf("cmd: must be native-incompatible, got %+v ok=%v", f, ok)
	}
	if f.Path != "containers.svc.cmd" {
		t.Errorf("Path = %q, want %q", f.Path, "containers.svc.cmd")
	}
}

func TestNativeIncompatible_KeepaliveFalseRejected(t *testing.T) {
	t.Parallel()
	keepaliveFalse := false
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Keepalive: &keepaliveFalse},
	}}
	f, ok := firstNativeIncompatibleFeature(wf)
	if !ok || f.Kind != "keepalive" {
		t.Fatalf("keepalive: false must be native-incompatible, got %+v ok=%v", f, ok)
	}
	if f.Path != "containers.svc.keepalive" {
		t.Errorf("Path = %q, want %q", f.Path, "containers.svc.keepalive")
	}
}

// keepalive: true (or unset) is not native-incompatible on its own — only an
// explicit false (native genuinely cannot honor "keep this container alive"
// since there's no container) is rejected.
func TestNativeIncompatible_KeepaliveTrueAccepted(t *testing.T) {
	t.Parallel()
	keepaliveTrue := true
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Keepalive: &keepaliveTrue},
	}}
	if f, ok := firstNativeIncompatibleFeature(wf); ok {
		t.Fatalf("keepalive: true must not be native-incompatible, got %+v", f)
	}
}

// F4b (AWF1065): firstContainerlessCodeStep / checkContainerlessRunCapability
// unit tests — direct, package-internal exercise of the pure walker + guard
// checkContainerlessRunCapability wires into cli/run.go and cli/resume.go
// (BareRunDocker* end-to-end coverage lives in cli/bare_run_docker_test.go).

func TestFirstContainerlessCodeStep_RootBareStep(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bare", Run: "true"},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}
	path, ok := firstContainerlessCodeStep(ld)
	if !ok || path != "bare" {
		t.Fatalf("firstContainerlessCodeStep = (%q, %v), want (\"bare\", true)", path, ok)
	}
}

// A CodeStep with a declared container: is not bare — only the code step
// with an empty Container is flagged, matching F4a's exact bareness test
// (cs.Container == "").
func TestFirstContainerlessCodeStep_NoneWhenAllDeclared(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{"lab": {Image: "x@sha256:" + strings.Repeat("0", 64)}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "declared", Container: "lab", Run: "true"},
		},
	}
	ld := &ir.LoadedDefinition{Workflow: wf}
	if path, ok := firstContainerlessCodeStep(ld); ok {
		t.Fatalf("firstContainerlessCodeStep = (%q, true), want ok=false (no bare step)", path)
	}
}

// A bare step nested under control-flow (If/Loop/Try/Parallel) must still be
// found — the walker recurses through ir.WalkNodes exactly like
// firstDockerOnlyFeature's sibling detectors, not a shallow top-level-only scan.
func TestFirstContainerlessCodeStep_NestedUnderIf(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{Cond: "true", Then: ir.NodeList{
			&ir.CodeStep{ID: "nested-bare", Run: "true"},
		}},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}
	path, ok := firstContainerlessCodeStep(ld)
	if !ok || path != "if[0].then.nested-bare" {
		t.Fatalf("firstContainerlessCodeStep = (%q, %v), want (\"if[0].then.nested-bare\", true)", path, ok)
	}
}

// A bare step reachable only through an imported/`call:`-resolved module must
// be found too — ld.Modules already holds every module (root + imports +
// call targets) after loader.Load, so a single ld.WalkModules pass (mirroring
// firstDockerOnlyFeatureForLoadedDefinition) is enough; no separate call-graph
// walk is needed. The "module <id> <path>" prefix matches
// firstDockerOnlyFeatureForLoadedDefinition's convention exactly.
func TestFirstContainerlessCodeStep_ReachableThroughImportedModule(t *testing.T) {
	t.Parallel()
	rootWF := &ir.Workflow{Containers: map[string]ir.Container{
		"lab": {Image: "x@sha256:" + strings.Repeat("0", 64)},
	}, Graph: ir.NodeList{
		&ir.CodeStep{ID: "declared", Container: "lab", Run: "true"},
	}}
	childWF := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "child-bare", Run: "true"},
	}}
	ld := &ir.LoadedDefinition{
		Workflow: rootWF,
		Modules: map[string]*ir.LoadedModule{
			"":      {ID: "", Workflow: rootWF},
			"recon": {ID: "recon", Workflow: childWF},
		},
	}
	path, ok := firstContainerlessCodeStep(ld)
	if !ok || path != "module recon child-bare" {
		t.Fatalf("firstContainerlessCodeStep = (%q, %v), want (\"module recon child-bare\", true)", path, ok)
	}
}

func TestCheckContainerlessRunCapability_BareRunDockerRejected(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bare", Run: "true"},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}
	err := checkContainerlessRunCapability(ld, engine.BackendDocker)
	if err == nil {
		t.Fatal("checkContainerlessRunCapability(docker) = nil, want AWF1065 rejection")
	}
	if !strings.Contains(err.Error(), "AWF1065") {
		t.Errorf("err = %q, want AWF1065", err)
	}
	if !strings.Contains(err.Error(), "bare") {
		t.Errorf("err = %q, want the offending step path %q", err, "bare")
	}
	// The guard's message is sourced from ir.CatalogText("AWF1065"), not a
	// hand-duplicated literal (see cli/backend_features.go) — assert the
	// catalog's own wording actually appears in the composed error, so the
	// two can never silently drift back apart.
	if want := ir.CatalogText("AWF1065"); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain ir.CatalogText(\"AWF1065\") = %q", err, want)
	}
}

func TestCheckContainerlessRunCapability_FakeAndNativeAccepted(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bare", Run: "true"},
	}}
	ld := &ir.LoadedDefinition{Workflow: wf}
	for _, kind := range []string{engine.BackendFake, engine.BackendNative} {
		if err := checkContainerlessRunCapability(ld, kind); err != nil {
			t.Errorf("checkContainerlessRunCapability(%q) = %v, want nil", kind, err)
		}
	}
}

// A declared-container-only workflow (no bare step at all) never rejects,
// regardless of the resolved backend — the guard is a no-op unless a bare
// step actually exists.
func TestCheckContainerlessRunCapability_NoBareStepAcceptedOnDocker(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{
		Containers: map[string]ir.Container{"lab": {Image: "x@sha256:" + strings.Repeat("0", 64)}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "declared", Container: "lab", Run: "true"},
		},
	}
	ld := &ir.LoadedDefinition{Workflow: wf}
	if err := checkContainerlessRunCapability(ld, engine.BackendDocker); err != nil {
		t.Errorf("checkContainerlessRunCapability(docker, no bare step) = %v, want nil", err)
	}
}
