// Package loader reads workflow YAML files and their referenced compose files/assets into an
// ir.LoadedDefinition. The I/O front for validation: file reads happen here so validation itself
// can be pure over an already-loaded snapshot.
//
// Paths declared by containers, imports, and assets go through two independent gates:
//
//  1. safeRootRelPath rejects absolute paths, paths containing backslashes (manifest paths are
//     slash-separated; silently rewriting them hides authoring mistakes), control characters, and
//     paths that escape the workflow directory after path.Clean.
//  2. os.Root (Go 1.24, rooted at the workflow directory) independently confines all opens to
//     the workflow directory. Root follows inside-root symlinks, so Load explicitly rejects
//     symlink path components before opening files.
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

	"github.com/valbaudo/awf/frontend/yaml"
	"github.com/valbaudo/awf/ir"
)

// Load reads the root workflow file at workflowPath, parses it into the IR, reads each referenced
// compose file and asset snapshot, then recursively loads local workflow imports. Root-level
// Workflow, WorkflowPath, ComposeFiles, and Assets fields remain aliases for the root module.
func Load(workflowPath string) (*ir.LoadedDefinition, error) {
	abs, err := filepath.Abs(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path %q: %w", workflowPath, err)
	}
	workflowDir := filepath.Dir(abs)
	rootDir, err := os.OpenRoot(workflowDir)
	if err != nil {
		return nil, &LoadError{Code: "AWF_IMPORT_READ", Source: abs, Message: "open workflow directory", Err: err}
	}
	defer func() { _ = rootDir.Close() }() // read-only Root; Close error not meaningful

	modules := map[string]*ir.LoadedModule{}
	var edges []ir.LoadedImportEdge
	root, err := loadModuleFromRoot(rootDir, filepath.Base(abs), abs, "", nil, 0, modules, &edges)
	if err != nil {
		return nil, err
	}
	// ir.ValidateJury runs BEFORE desugarJury: it inspects the pre-desugar jury:
	// blocks (output_schema present, uniform over: keys, a resolvable field:
	// default, and gate.evaluate-terminal placement). ir.Validate can never see
	// these checks — it runs after Load has already desugared jury: away — so
	// this loader-side pass is the only place they can live. Reject on the
	// first violation (deterministic: lowest path, then code) rather than
	// silently desugaring a malformed block into a misleading downstream error
	// (e.g. an unresolved field: would otherwise surface as AWF1035 on the
	// emitted map, not AWF1071 on the jury: block that caused it).
	//
	// modules is a map, so `for range modules` iterates in Go's randomized
	// order; collecting every module's diagnostics into ONE slice before
	// picking "the first" (juryLoadError sorts by path, then code) keeps the
	// choice deterministic regardless of module count or iteration order —
	// and, as a side benefit, a violation in an imported module is never
	// masked by the root module validating clean first.
	var juryErrs []ir.Diagnostic
	for _, m := range modules {
		juryErrs = append(juryErrs, ir.ValidateJury(m.Workflow)...)
	}
	if len(juryErrs) > 0 {
		return nil, juryLoadError(juryErrs)
	}

	// F45: default an OMITTED `concurrency:` to 1 (serial) BEFORE any digest or validation
	// pass ever sees the IR, so an omitted concurrency: and an explicit `concurrency: 1`
	// normalize to byte-identical IR (same digest). Runs over every module — root AND every
	// imported workflow — since modules[""] is the root module (loadModuleFromRoot above
	// keys it that way) and its Workflow is the SAME pointer aliased into the returned
	// LoadedDefinition.Workflow below.
	//
	// desugarJury runs FIRST: a jury: block lowers to a map, and that map's
	// omitted concurrency: must still default to 1 like any other map, so the
	// desugared and hand-written map+quorum forms normalize to byte-identical IR.
	for _, m := range modules {
		desugarJury(m.Workflow)
		applyMapConcurrencyDefault(m.Workflow)
	}
	return &ir.LoadedDefinition{
		Workflow:     root.Workflow,
		WorkflowPath: root.WorkflowPath,
		ComposeFiles: root.ComposeFiles,
		Assets:       root.Assets,
		Modules:      modules,
		ImportEdges:  edges,
	}, nil
}

// applyMapConcurrencyDefault walks every node in wf.Graph and sets a nil `Map.Concurrency`
// (F45: `concurrency:` was omitted from the source) to a pointer to 1 (serial), in place.
// ir.WalkNodes recurses into every nested body (map/loop/if/try/parallel/gate/compose), so
// nested maps are covered too. Must run before ComputeDigest/ir.Validate — see the call site
// in Load — so the normalized IR (not the raw decode) is what gets digested/validated.
func applyMapConcurrencyDefault(wf *ir.Workflow) {
	if wf == nil {
		return
	}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, _ string) {
		m, ok := n.(*ir.Map)
		if !ok || m.Concurrency != nil {
			return
		}
		one := 1
		m.Concurrency = &one
	})
}

func loadModuleFromRoot(
	root *os.Root,
	workflowRel string,
	abs string,
	moduleID string,
	stack []string,
	depth int,
	modules map[string]*ir.LoadedModule,
	edges *[]ir.LoadedImportEdge,
) (*ir.LoadedModule, error) {
	if depth > maxImportDepth {
		return nil, &LoadError{
			Code:    "AWF_IMPORT_DEPTH",
			Source:  abs,
			Message: fmt.Sprintf("import depth exceeds maximum %d", maxImportDepth),
		}
	}
	for _, seen := range stack {
		if seen == abs {
			return nil, &LoadError{
				Code:    "AWF_IMPORT_CYCLE",
				Source:  abs,
				Message: "workflow import cycle detected",
			}
		}
	}

	wfBytes, err := readRootRegularFile(root, workflowRel)
	if err != nil {
		return nil, &LoadError{Code: "AWF_IMPORT_READ", Source: abs, Path: "workflow", Message: "read workflow", Err: err}
	}
	wf, raw, err := yaml.DecodeWithRaw(wfBytes)
	if err != nil {
		return nil, &LoadError{Code: "AWF_IMPORT_DECODE", Source: abs, Message: "decode workflow YAML", Err: err}
	}

	compose, err := loadComposeFiles(root, wf)
	if err != nil {
		return nil, err
	}

	assets, err := loadAssets(root, wf.Assets)
	if err != nil {
		return nil, err
	}
	for id, asset := range assets {
		wf.Assets[id] = asset.DeclaredPath
	}

	module := &ir.LoadedModule{
		ID:           moduleID,
		Workflow:     wf,
		WorkflowPath: abs,
		ComposeFiles: compose,
		Assets:       assets,
		RawDoc:       raw,
	}
	modules[moduleID] = module
	nextStack := append(append([]string(nil), stack...), abs)
	if err := loadImports(module, root, nextStack, depth, modules, edges); err != nil {
		return nil, err
	}
	return module, nil
}

func loadComposeFiles(root *os.Root, wf *ir.Workflow) (map[string][]byte, error) {
	compose := map[string][]byte{}
	for name, c := range wf.Containers {
		if c.Compose == "" {
			continue
		}
		rel, err := composeRelPath(c.Compose)
		if err != nil {
			return nil, fmt.Errorf("container %q compose %q: %w", name, c.Compose, err)
		}
		// If two containers reference the same cleaned path (e.g. "./lab/compose.yml" and
		// "lab/compose.yml"), read the file once. Avoids a TOCTOU window where two reads of
		// the same path could see different bytes, which would destabilize the future
		// compose-fold digest. Still normalize Container.Compose for the second container.
		if _, seen := compose[rel]; seen {
			c.Compose = rel
			wf.Containers[name] = c
			continue
		}
		if err := rejectSymlinkComponents(root, rel); err != nil {
			return nil, fmt.Errorf("container %q compose %q: %w", name, c.Compose, err)
		}
		// os.Root.Open enforces no `..`-escape; composeRelPath has already rejected
		// absolute paths and backslashes. A missing file surfaces here as fs.ErrNotExist.
		f, err := root.Open(rel)
		if err != nil {
			return nil, fmt.Errorf("container %q compose %q: %w", name, c.Compose, err)
		}
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("container %q compose %q: stat: %w", name, c.Compose, statErr)
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("container %q compose %q: not a regular file", name, c.Compose)
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
	return compose, nil
}

func readRootRegularFile(root *os.Root, rel string) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	b, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close: %w", closeErr)
	}
	return b, nil
}

// composeRelPath validates and normalizes a compose path declared in a container.
func composeRelPath(declared string) (string, error) {
	return safeRootRelPath(declared, safePathPolicy{kind: "compose"})
}
