// Package loader reads a workflow YAML file and its referenced compose files into an
// ir.LoadedDefinition. The I/O front for validation: file reads happen here so validation itself
// can be pure over an already-loaded snapshot.
//
// Compose paths declared by containers go through two independent gates:
//
//  1. composeRelPath rejects absolute paths, paths containing backslashes (compose paths are
//     POSIX; backslashes have no legitimate use and silently rewriting them hides authoring
//     mistakes), and paths that escape the workflow directory after filepath.Clean.
//  2. os.Root (Go 1.24, rooted at the workflow directory) refuses any escape that slipped past
//     gate 1 — including any symlink in the resolution path, regardless of where it resolves
//     (Load is conservative: symlinks are refused even when they target a sibling file inside
//     the rooted directory).
//
// Load also normalizes each Container.Compose to its cleaned forward-slash form so the IR field
// and the ComposeFiles map key agree (this matters for the spec §E compose-fold and for any
// caller looking up bytes by container).
package loader

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/frontend/yaml"
	"github.com/valbaudo/awf/ir"
)

// Load reads the workflow file at workflowPath, parses it into the IR, then reads each referenced
// compose file. Returns an error on the first failure (workflow unreadable, YAML parse error,
// compose-path escape, compose missing/unreadable). Slice 1.4 may later map these to typed
// diagnostics; for now they propagate as Go errors with attribution.
func Load(workflowPath string) (*ir.LoadedDefinition, error) {
	abs, err := filepath.Abs(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path %q: %w", workflowPath, err)
	}
	wfBytes, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", abs, err)
	}
	wf, err := yaml.Decode(wfBytes)
	if err != nil {
		return nil, fmt.Errorf("decode %q: %w", abs, err)
	}

	workflowDir := filepath.Dir(abs)
	root, err := os.OpenRoot(workflowDir)
	if err != nil {
		return nil, fmt.Errorf("open workflow dir %q as root: %w", workflowDir, err)
	}
	defer root.Close()

	compose := map[string][]byte{}
	for name, c := range wf.Containers {
		if c.Compose == "" {
			continue
		}
		rel, err := composeRelPath(c.Compose)
		if err != nil {
			return nil, fmt.Errorf("container %q compose %q: %w", name, c.Compose, err)
		}
		// os.Root.Open enforces no `..`-escape and refuses every symlink in the resolution
		// path (even inside-root ones); composeRelPath has already rejected absolute paths
		// and backslashes. A missing file surfaces here as fs.ErrNotExist.
		f, err := root.Open(rel)
		if err != nil {
			return nil, fmt.Errorf("container %q compose %q: %w", name, c.Compose, err)
		}
		b, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("container %q compose %q: read: %w", name, c.Compose, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("container %q compose %q: close: %w", name, c.Compose, closeErr)
		}
		compose[rel] = b

		// Normalize Container.Compose in-place to the cleaned forward-slash form so the IR
		// field and the ComposeFiles map key agree. Go map struct values aren't directly
		// mutable, so the read-modify-write triplet is required.
		c.Compose = rel
		wf.Containers[name] = c
	}

	return &ir.LoadedDefinition{
		Workflow:     wf,
		WorkflowPath: abs,
		ComposeFiles: compose,
	}, nil
}

// composeRelPath validates and normalizes a compose path declared in a container. The path must
// be relative (no absolute), must not contain backslashes (compose paths are POSIX; on darwin/
// linux filepath.IsAbs is false for "\foo" and filepath.Clean leaves backslashes alone, so a
// "..\..\escape" string would otherwise survive the prefix check and reach os.Root with only an
// opaque "no such file" error — reject it up front for honest attribution and cross-OS parity),
// and must not escape the workflow directory after Clean (no leading "../"). The returned form
// is forward-slashed for use both as the os.Root.Open path and as the ComposeFiles map key.
//
// os.Root would itself reject `..`-escape and refuse any symlink, but checking here gives clearer,
// attributed error messages distinct from "file not found".
func composeRelPath(declared string) (string, error) {
	if filepath.IsAbs(declared) {
		return "", fmt.Errorf("absolute path not permitted (must be relative to the workflow directory)")
	}
	if strings.ContainsRune(declared, '\\') {
		return "", fmt.Errorf("backslash not permitted in compose paths; use forward slash")
	}
	clean := filepath.ToSlash(filepath.Clean(declared))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes the workflow directory after cleaning: %q", clean)
	}
	return clean, nil
}
