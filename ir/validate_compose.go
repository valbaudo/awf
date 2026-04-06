package ir

import (
	"context"
	"fmt"
	"strings"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	goyaml "github.com/goccy/go-yaml"
)

// validateCompose runs the AWF3003 (digest-pinning of inner images), AWF3004 (compose parse
// failure), and AWF3005 (extends/include directives forbidden) pass. We use compose-spec/
// compose-go/v2 to parse each ld.ComposeFiles[k] from bytes — honoring CLAUDE.md's "don't
// reinvent docker" boundary — and then walk the resulting Project's Services map.
//
// Per AWF standard §3: every service `image:` MUST satisfy `strings.Contains(img, "@sha256:")`.
// A service with no `image:` (e.g. `build:` only) is also rejected as AWF3003 because AWF
// can't pin what isn't there.
//
// SECURITY: compose-go honors several file-following directives (extends, include, env_file)
// by reading those paths from disk — INCLUDING absolute paths, INCLUDING outside the workflow
// directory. Empirically verified: `extends: file: /etc/passwd` and `env_file: /etc/hosts`
// both attempt the reads. Slice 1.2's os.Root confinement governs the loader's READ of the
// workflow-declared compose path, but not compose-go's transitive reads. Defense in depth:
//
//	(1) Pre-scan the compose YAML (decode to map[string]any via goccy/go-yaml) for top-level
//	    `include:` or a service-level `extends:` key. If found → AWF3005, skip the rest.
//	(2) Configure the compose-go loader with SkipExtends, SkipInclude, AND
//	    SkipResolveEnvironment as defense-in-depth: a future compose-spec extension or a
//	    pre-scan miss never reopens the file-read primitive. env_file: is NOT pre-scanned-
//	    rejected because it's a legitimate compose feature for runtime env injection —
//	    SkipResolveEnvironment simply means the validator doesn't read those files (the
//	    runtime container backend does, at execution time, in Phase 4).
//
// AWF3004 is for catastrophic parse failures (malformed YAML, references to undefined
// anchors, etc.); the underlying message is preserved for attribution. compose-go's deeper
// consistency checks (port conflicts, network refs, depends_on cycles) deliberately swallow
// into AWF3004 — the resume-integrity property AWF cares about is digest-pinning, not the
// full compose-spec.
func validateCompose(ld *LoadedDefinition, c *collector) {
	for name, ctr := range ld.Workflow.Containers {
		if ctr.Compose == "" {
			continue
		}
		// loader.Load normalized Container.Compose to the cleaned forward-slash form, which
		// is also the ComposeFiles map key (slice 1.2 contract).
		bytes, ok := ld.ComposeFiles[ctr.Compose]
		if !ok {
			// Missing compose file — but loader.Load would have errored before producing
			// the LoadedDefinition. This branch is defensive: surface as AWF3004 if seen.
			c.errf(ContainerPath(name, ""), "AWF3004", fmt.Sprintf("compose file %q not loaded", ctr.Compose))
			continue
		}
		// Layer 1: pre-scan for extends/include directives. Refuse rather than try to
		// validate them — they're a file-read primitive disguised as a portability feature.
		if found, reason := hasExtendsOrInclude(bytes); found {
			c.errf(ContainerPath(name, ""), "AWF3005", reason)
			continue
		}
		// Layer 2: compose-go with SkipExtends + SkipInclude as defense-in-depth.
		project, err := loadComposeBytes(ctr.Compose, bytes)
		if err != nil {
			c.errf(ContainerPath(name, ""), "AWF3004", err.Error())
			continue
		}
		for svcName, svc := range project.Services {
			if svc.Image == "" {
				c.errf(ContainerPath(name, ""), "AWF3003",
					fmt.Sprintf("service %q has no image: (AWF cannot digest-pin a build target)", svcName))
				continue
			}
			if !strings.Contains(svc.Image, "@sha256:") {
				c.errf(ContainerPath(name, ""), "AWF3003",
					fmt.Sprintf("service %q image %q is not digest-pinned", svcName, svc.Image))
			}
		}
	}
}

// hasExtendsOrInclude scans the raw compose YAML for the two file-following directives the
// validator refuses (AWF3005). Returns (true, reason) if either appears. Structural decode
// via goccy/go-yaml so the detection isn't fooled by the literal string "extends:" appearing
// in a comment or service name.
func hasExtendsOrInclude(content []byte) (bool, string) {
	var raw any
	if err := goyaml.Unmarshal(content, &raw); err != nil {
		// Malformed YAML — AWF3004 will surface the same load failure with a richer message,
		// so don't double-report here.
		return false, ""
	}
	root, ok := raw.(map[string]any)
	if !ok {
		return false, ""
	}
	// Top-level include: directive.
	if _, has := root["include"]; has {
		return true, catalog["AWF3005"] + " (top-level `include:` found)"
	}
	// service-level extends: directive (every service is a map, the key we look for is `extends`).
	if services, ok := root["services"].(map[string]any); ok {
		for svcName, raw := range services {
			if svc, ok := raw.(map[string]any); ok {
				if _, has := svc["extends"]; has {
					return true, fmt.Sprintf("%s (service %q has `extends:`)", catalog["AWF3005"], svcName)
				}
			}
		}
	}
	return false, ""
}

// loadComposeBytes parses a compose file from in-memory bytes with SkipExtends and SkipInclude
// set — see the validateCompose doc comment for the security rationale.
func loadComposeBytes(filename string, bytes []byte) (*composetypes.Project, error) {
	ctx := context.Background()
	return composeloader.LoadWithContext(ctx,
		composetypes.ConfigDetails{
			ConfigFiles: []composetypes.ConfigFile{{Content: bytes, Filename: filename}},
		},
		func(opts *composeloader.Options) {
			opts.SkipValidation = false
			opts.SkipConsistencyCheck = true
			opts.SkipExtends = true            // defense-in-depth — pre-scan above is the primary refusal
			opts.SkipInclude = true            // defense-in-depth — pre-scan above is the primary refusal
			opts.SkipResolveEnvironment = true // prevents env_file: reads — same SSRF-class primitive as extends/include
			opts.SetProjectName("awf-validate", true)
		},
	)
}
