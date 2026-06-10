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
	if ld == nil {
		return dockerFeature{}, false
	}
	var out dockerFeature
	var found bool
	_ = ld.WalkModules(func(module *ir.LoadedModule) error {
		if found || module == nil {
			return nil
		}
		feature, ok := firstDockerOnlyFeature(module.Workflow)
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
		if feature, ok := firstDockerOnlyFeatureForLoadedDefinition(ld); ok {
			return "", fmt.Errorf("--backend native cannot run Docker-only feature %q at %s; use --backend docker", feature.Kind, feature.Path)
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
