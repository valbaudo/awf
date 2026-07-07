package cli

import (
	"fmt"
	"sort"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

const backendAuto = "auto"

type dockerFeature struct {
	Kind string
	Path string
}

func firstDockerOnlyFeature(wf *ir.Workflow) (dockerFeature, bool) {
	if wf == nil {
		return dockerFeature{}, false
	}
	names := make([]string, 0, len(wf.Containers))
	for name := range wf.Containers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := wf.Containers[name]
		if c.Snapshot == "workspace" {
			return dockerFeature{Kind: "snapshot: workspace", Path: fmt.Sprintf("containers.%s.snapshot", name)}, true
		}
		if c.Image != "" {
			return dockerFeature{Kind: "static container image", Path: fmt.Sprintf("containers.%s.image", name)}, true
		}
		if c.Compose != "" {
			return dockerFeature{Kind: "static compose", Path: fmt.Sprintf("containers.%s.compose", name)}, true
		}
	}
	if path, ok := ir.FirstRuntimeComposePath(wf); ok {
		return dockerFeature{Kind: "runtime compose", Path: path}, true
	}
	targets := ir.MapImageTargets(wf)
	if len(targets) > 0 {
		names := make([]string, 0, len(targets))
		for name := range targets {
			names = append(names, name)
		}
		sort.Strings(names)
		return dockerFeature{Kind: "runtime map image", Path: fmt.Sprintf("containers.%s", names[0])}, true
	}
	return dockerFeature{}, false
}

func firstDockerOnlyFeatureForLoadedDefinition(ld *ir.LoadedDefinition) (dockerFeature, bool) {
	return firstFeatureForLoadedDefinition(ld, firstDockerOnlyFeature)
}

// firstContainerlessCodeStep reports the static IR path of the first
// container-less (bare `run:`) *ir.CodeStep reachable from ld — the root
// module plus every imported / `call:`-reached module. ld.Modules already
// resolves call: targets at load time (loader.Load), so a single
// ld.WalkModules pass covers both without a separate call-graph walk; this
// mirrors firstDockerOnlyFeatureForLoadedDefinition's module-walk /
// "module <id> <path>" prefixing shape exactly, just over ir.WalkNodes'
// step-level tree instead of wf.Containers.
//
// F4a gives a bare CodeStep (Container == "") a per-step implicit
// host-workspace handle at dispatch time (engine/interpreter.go,
// hostWorkspaceSpec) — that handle carries no image, so it cannot run under
// docker. AWF1065's run-start guard (checkContainerlessRunCapability, below)
// uses this to fail closed before a bare step ever reaches a docker-resolved
// dispatcher.
//
// Only *ir.CodeStep is checked: *ir.AgentStep's empty Container means a
// Containerless-capable adapter (awf/llm), a different mechanism already
// guarded elsewhere (AWF1057-adjacent); *ir.Reduce's run: form REQUIRES
// container: (AWF1035, ir/validate_reduce.go), and a *ir.Map's own
// container: is required too (AWF1012) — neither can ever be bare.
func firstContainerlessCodeStep(ld *ir.LoadedDefinition) (string, bool) {
	if ld == nil {
		return "", false
	}
	var out string
	var found bool
	_ = ld.WalkModules(func(module *ir.LoadedModule) error {
		if found || module == nil || module.Workflow == nil {
			return nil
		}
		var path string
		var hit bool
		ir.WalkNodes(module.Workflow.Graph, "", func(n ir.Node, p string) {
			if hit {
				return
			}
			if cs, ok := n.(*ir.CodeStep); ok && cs.Container == "" {
				path = p
				hit = true
			}
		})
		if !hit {
			return nil
		}
		if module.ID != "" {
			path = fmt.Sprintf("module %s %s", module.ID, path)
		}
		out = path
		found = true
		return nil
	})
	return out, found
}

// checkContainerlessRunCapability rejects, at run/resume start, a workflow
// that resolves to the docker backend (explicit --backend docker, or auto's
// image/snapshot/compose-triggered resolution via
// firstDockerOnlyFeatureForLoadedDefinition) while it also contains a
// container-less (bare `run:`) CodeStep — AWF1065.
//
// Deliberately a run-start CLI check, not a static ir.Validate rule: whether
// a bare step is a PROBLEM depends on the resolved --backend, which validate
// never sees. It must run AFTER backend resolution (selectRunBackendForLoadedDefinition)
// so a MIXED workflow — an image-backed container alongside a bare step,
// which auto-resolves to docker precisely because of that image — is caught
// too, not just an explicit `--backend docker`. native and fake both pass
// through untouched (F4a already makes them bare-run-capable).
func checkContainerlessRunCapability(ld *ir.LoadedDefinition, backendKind string) error {
	if backendKind != engine.BackendDocker {
		return nil
	}
	path, ok := firstContainerlessCodeStep(ld)
	if !ok {
		return nil
	}
	// Message text mirrors catalog["AWF1065"] (ir/diagnostic.go) verbatim; cli
	// cannot reference the unexported catalog map directly, so it is
	// deliberately duplicated here — keep the two in sync on edit (same
	// cross-package pattern as the AWF1025 reference comment in cli/run.go).
	return fmt.Errorf("AWF1065: %s: a container-less `run:` step requires native execution; it is incompatible with `--backend docker` — declare a `container:` or run native", path)
}

// firstNativeIncompatibleFeature detects only the features native genuinely
// cannot run — static compose (native Create refuses compose), runtime compose
// (ir.FirstRuntimeComposePath), runtime map image (native Caps.RuntimeImage is
// false), cmd: (an explicit standing service command — a keepalive sidecar),
// and keepalive: false (F5, U4: neither has a host equivalent — native has no
// container to run a standing command in, or to not keep alive). Unlike
// firstDockerOnlyFeature it deliberately does NOT flag a static c.Image,
// c.Resources, or snapshot: workspace: native CAN run those, just without
// isolation or resource limits — it ignores the declared image/resources,
// runs steps on the host, and snapshots the workdir. The "docker-preferred but
// native-runnable" set is left for auto (which still prefers docker for
// reproducibility).
func firstNativeIncompatibleFeature(wf *ir.Workflow) (dockerFeature, bool) {
	if wf == nil {
		return dockerFeature{}, false
	}
	names := make([]string, 0, len(wf.Containers))
	for name := range wf.Containers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := wf.Containers[name]
		if c.Compose != "" {
			return dockerFeature{Kind: "static compose", Path: fmt.Sprintf("containers.%s.compose", name)}, true
		}
		// F5 (U4): cmd: (an explicit service command — a keepalive sidecar)
		// and keepalive: false (asking native to not keep a container alive)
		// both have no host equivalent — native has no container to run a
		// standing command in, or to not keep alive. Fail closed rather than
		// silently no-op them.
		if len(c.Cmd) > 0 {
			return dockerFeature{Kind: "cmd", Path: fmt.Sprintf("containers.%s.cmd", name)}, true
		}
		if c.Keepalive != nil && !*c.Keepalive {
			return dockerFeature{Kind: "keepalive", Path: fmt.Sprintf("containers.%s.keepalive", name)}, true
		}
	}
	if path, ok := ir.FirstRuntimeComposePath(wf); ok {
		return dockerFeature{Kind: "runtime compose", Path: path}, true
	}
	targets := ir.MapImageTargets(wf)
	if len(targets) > 0 {
		names := make([]string, 0, len(targets))
		for name := range targets {
			names = append(names, name)
		}
		sort.Strings(names)
		return dockerFeature{Kind: "runtime map image", Path: fmt.Sprintf("containers.%s", names[0])}, true
	}
	return dockerFeature{}, false
}

func firstNativeIncompatibleFeatureForLoadedDefinition(ld *ir.LoadedDefinition) (dockerFeature, bool) {
	return firstFeatureForLoadedDefinition(ld, firstNativeIncompatibleFeature)
}

// firstFeatureForLoadedDefinition walks the loaded modules in order, returning
// the first feature detect reports, prefixing imported-module paths with the
// module id. detect is firstDockerOnlyFeature (auto) or
// firstNativeIncompatibleFeature (explicit native).
func firstFeatureForLoadedDefinition(ld *ir.LoadedDefinition, detect func(*ir.Workflow) (dockerFeature, bool)) (dockerFeature, bool) {
	if ld == nil {
		return dockerFeature{}, false
	}
	var out dockerFeature
	var found bool
	_ = ld.WalkModules(func(module *ir.LoadedModule) error {
		if found || module == nil {
			return nil
		}
		feature, ok := detect(module.Workflow)
		if !ok {
			return nil
		}
		if module.ID != "" {
			feature.Path = fmt.Sprintf("module %s %s", module.ID, feature.Path)
		}
		out = feature
		found = true
		return nil
	})
	return out, found
}

// workflowHasStaticImage reports whether any module declares a static
// container image. Native ignores declared images (it runs steps on the host
// with no isolation), so the run path uses this to emit a non-silent warning
// rather than silently dropping the declared image.
func workflowHasStaticImage(ld *ir.LoadedDefinition) bool {
	if ld == nil {
		return false
	}
	var found bool
	_ = ld.WalkModules(func(module *ir.LoadedModule) error {
		if found || module == nil || module.Workflow == nil {
			return nil
		}
		for _, c := range module.Workflow.Containers {
			if c.Image != "" {
				found = true
				return nil
			}
		}
		return nil
	})
	return found
}

// workflowHasResources reports whether any module declares container
// resources: (cpu/mem). Native ignores resource limits (steps run directly
// on the host, no cgroup/isolation layer to apply them to), so the run path
// uses this — alongside workflowHasStaticImage — to enumerate every key the
// non-silent native warning names (F5, U4).
func workflowHasResources(ld *ir.LoadedDefinition) bool {
	if ld == nil {
		return false
	}
	var found bool
	_ = ld.WalkModules(func(module *ir.LoadedModule) error {
		if found || module == nil || module.Workflow == nil {
			return nil
		}
		for _, c := range module.Workflow.Containers {
			if c.Resources != nil {
				found = true
				return nil
			}
		}
		return nil
	})
	return found
}

func selectRunBackend(requested string, wf *ir.Workflow) (string, error) {
	return selectRunBackendForLoadedDefinition(requested, &ir.LoadedDefinition{Workflow: wf})
}

func selectRunBackendForLoadedDefinition(requested string, ld *ir.LoadedDefinition) (string, error) {
	switch requested {
	case backendAuto:
		if _, ok := firstDockerOnlyFeatureForLoadedDefinition(ld); ok {
			return engine.BackendDocker, nil
		}
		return engine.BackendNative, nil
	case engine.BackendNative:
		// Explicit native is permissive: it runs static image and
		// snapshot: workspace workflows on the host (ignoring the declared
		// image, snapshotting the workdir). It still refuses the features
		// native genuinely cannot do — compose, runtime compose, runtime map
		// image, cmd:, and keepalive: false (F5, U4). (Auto, above, still
		// routes image/snapshot to docker for reproducibility.)
		if feature, ok := firstNativeIncompatibleFeatureForLoadedDefinition(ld); ok {
			return "", fmt.Errorf("--backend native cannot run %q at %s; use --backend docker", feature.Kind, feature.Path)
		}
		return engine.BackendNative, nil
	case engine.BackendDocker, engine.BackendFake:
		return requested, nil
	default:
		return "", fmt.Errorf("invalid --backend value %q; want %q, %q, %q, or %q", requested, backendAuto, engine.BackendNative, engine.BackendDocker, engine.BackendFake)
	}
}

func checkWorkflowBackendCapabilities(ld *ir.LoadedDefinition, backendKind string, backend container.Backend) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil {
			return nil
		}
		if err := checkWorkflowModuleBackendCapabilities(module.Workflow, backendKind, backend); err != nil {
			if module.ID != "" {
				return fmt.Errorf("module %s: %w", module.ID, err)
			}
			return err
		}
		return nil
	})
}

func checkWorkflowModuleBackendCapabilities(wf *ir.Workflow, backendKind string, backend container.Backend) error {
	if err := checkSnapshotCapability(wf, backend); err != nil {
		return err
	}
	if err := checkRuntimeImageCapability(wf, backend); err != nil {
		return err
	}
	if err := checkRuntimeComposeCapability(wf, backendKind, backend); err != nil {
		return err
	}
	return nil
}
