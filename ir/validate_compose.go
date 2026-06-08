package ir

import (
	"context"
	"fmt"
	"strings"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	goyaml "github.com/goccy/go-yaml"
	digest "github.com/opencontainers/go-digest"
)

type ComposeValidationError struct {
	Code    string
	Message string
}

// validateCompose runs the AWF3003 (digest-pinning of inner images), AWF3004 (compose parse
// failure), and AWF3005 (extends/include directives forbidden) pass. We use compose-spec/
// compose-go/v2 to parse each ld.ComposeFiles[k] from bytes — honoring CLAUDE.md's "don't
// reinvent docker" boundary — and then walk the resulting Project's Services map.
//
// Per AWF standard §3: every service `image:` MUST satisfy `strings.Contains(img, "@sha256:")`.
// A service with no `image:` (e.g. `build:` only) is also rejected as AWF3003 because AWF
// can't pin what isn't there.
//
// SECURITY: compose-go honors several file-following directives (extends, include, env_file,
// label_file) by reading those paths from disk — INCLUDING absolute paths, INCLUDING outside
// the workflow directory. Empirically verified: `extends: file: /etc/passwd` and `env_file:
// /etc/hosts` both attempt the reads. compose-go v2.11.0 exposes SkipExtends/SkipInclude/
// SkipResolveEnvironment but NOT a SkipResolveLabels, so label_file's only defense is the
// pre-scan layer. Slice 1.2's os.Root confinement governs the loader's READ of the
// workflow-declared compose path, but not compose-go's transitive reads. Defense in depth:
//
//	(1) Pre-scan the compose YAML (decode to map[string]any via goccy/go-yaml) for top-level
//	    `include:` or a service-level `extends:` or `label_file:` key. If found → AWF3005,
//	    skip the rest. label_file is in the pre-scan because compose-go has no Skip option
//	    for it; extends/include are in the pre-scan as primary refusal even though they have
//	    Skip options (defense in depth).
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
		for _, err := range ValidateComposeBytes(ctr.Compose, bytes) {
			c.errf(ContainerPath(name, ""), err.Code, err.Message)
		}
	}
}

func ValidateComposeBytes(filename string, bytes []byte, requiredServices ...string) []ComposeValidationError {
	// Layer 1: pre-scan for extends/include/label_file directives. Refuse rather than try to
	// validate them — they're a file-read primitive disguised as a portability feature.
	if found, reason := hasFileFollowingDirective(bytes); found {
		return []ComposeValidationError{{Code: "AWF3005", Message: reason}}
	}
	// Layer 2: compose-go with SkipExtends + SkipInclude as defense-in-depth.
	project, err := loadComposeBytes(filename, bytes)
	if err != nil {
		return []ComposeValidationError{{Code: "AWF3004", Message: err.Error()}}
	}

	var errs []ComposeValidationError
	for svcName, svc := range project.Services {
		if svc.Image == "" {
			errs = append(errs, ComposeValidationError{
				Code:    "AWF3003",
				Message: fmt.Sprintf("service %q has no image: (AWF cannot digest-pin a build target)", svcName),
			})
			continue
		}
		if !validSHA256DigestPinnedImage(svc.Image) {
			errs = append(errs, ComposeValidationError{
				Code:    "AWF3003",
				Message: fmt.Sprintf("service %q image %q is not pinned to a valid sha256 digest", svcName, svc.Image),
			})
		}
	}
	for _, svcName := range requiredServices {
		svcName = strings.TrimSpace(svcName)
		if svcName == "" {
			errs = append(errs, ComposeValidationError{Code: "AWF3008", Message: catalog["AWF3008"] + ": empty service name"})
			continue
		}
		if _, ok := project.Services[svcName]; !ok {
			errs = append(errs, ComposeValidationError{
				Code:    "AWF3008",
				Message: fmt.Sprintf("%s: service %q not found", catalog["AWF3008"], svcName),
			})
		}
	}
	return errs
}

func validSHA256DigestPinnedImage(image string) bool {
	at := strings.LastIndex(image, "@")
	if at < 0 || at == len(image)-1 {
		return false
	}
	d, err := digest.Parse(image[at+1:])
	return err == nil && d.Algorithm() == digest.SHA256
}

// hasFileFollowingDirective scans the raw compose YAML for file-following directives the
// validator refuses (AWF3005). Returns (true, reason) if any appears. Structural decode via
// goccy/go-yaml so the detection isn't fooled by the literal strings appearing in comments
// or service names.
//
// The three directives covered:
//   - top-level `include:` (file inclusion)
//   - service-level `extends:` (config inheritance from another file)
//   - service-level `label_file:` (read labels from a file — compose-go has no Skip option for
//     this in v2.11.0, so the pre-scan is the only defense against absolute paths like
//     /etc/shadow being opened during validation)
func hasFileFollowingDirective(content []byte) (bool, string) {
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
	// service-level extends: and label_file: directives.
	if services, ok := root["services"].(map[string]any); ok {
		for svcName, raw := range services {
			svc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, has := svc["extends"]; has {
				return true, fmt.Sprintf("%s (service %q has `extends:`)", catalog["AWF3005"], svcName)
			}
			if _, has := svc["label_file"]; has {
				return true, fmt.Sprintf("%s (service %q has `label_file:` — compose-go reads it unconditionally)", catalog["AWF3005"], svcName)
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
