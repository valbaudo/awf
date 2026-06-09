package ir

import "sort"

// LoadedDefinition is the loader's output and the validator's input — the parsed Workflow plus the
// raw bytes of every referenced compose file, keyed by the cleaned workflow-relative forward-slash
// path. That key form is what ir/digest.go consumes for the spec §E compose-fold (length-prefixed
// (path, sha256(bytes)) entries, sorted by path, folded into the workflow's content digest).
//
// Lives in the `ir` package because validation — also in `ir` per docs/runtime-design.md —
// consumes it. Putting LoadedDefinition in `loader` would create a cycle (loader already imports
// ir for Workflow/Container/Node).
type LoadedDefinition struct {
	// Workflow is the parsed IR. Non-nil if Load succeeded.
	Workflow *Workflow

	// WorkflowPath is the absolute path of the workflow file (for error attribution).
	WorkflowPath string

	// ComposeFiles maps each container's cleaned workflow-relative compose path
	// (e.g. "lab/compose.yml") to the raw bytes the loader read. Forward-slashed regardless
	// of OS so the digest input is portable.
	ComposeFiles map[string][]byte

	// Assets maps each top-level asset id to the run-start snapshot loaded from disk.
	Assets map[string]LoadedAsset

	// Modules maps logical module ids to loaded workflow modules. The root module id is "".
	Modules map[string]*LoadedModule

	// ImportEdges records resolved parent -> child import relationships.
	ImportEdges []LoadedImportEdge
}

type LoadedModule struct {
	ID           string
	Workflow     *Workflow
	WorkflowPath string
	ComposeFiles map[string][]byte
	Assets       map[string]LoadedAsset
}

type LoadedImportEdge struct {
	ParentID     string
	ImportID     string
	DeclaredPath string
	ChildID      string
}

func (ld *LoadedDefinition) Root() *LoadedModule {
	if ld == nil {
		return nil
	}
	if ld.Modules != nil {
		if root, ok := ld.Modules[""]; ok {
			return root
		}
	}
	return &LoadedModule{
		ID:           "",
		Workflow:     ld.Workflow,
		WorkflowPath: ld.WorkflowPath,
		ComposeFiles: ld.ComposeFiles,
		Assets:       ld.Assets,
	}
}

func (ld *LoadedDefinition) Module(id string) (*LoadedModule, bool) {
	if ld == nil {
		return nil, false
	}
	if ld.Modules != nil {
		m, ok := ld.Modules[id]
		return m, ok
	}
	if id == "" {
		return ld.Root(), true
	}
	return nil, false
}

func (ld *LoadedDefinition) WalkModules(fn func(*LoadedModule) error) error {
	if ld == nil {
		return nil
	}
	if ld.Modules == nil {
		return fn(ld.Root())
	}
	ids := make([]string, 0, len(ld.Modules))
	for id := range ld.Modules {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i] == "" {
			return true
		}
		if ids[j] == "" {
			return false
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		if err := fn(ld.Modules[id]); err != nil {
			return err
		}
	}
	return nil
}

func (ld *LoadedDefinition) WalkImportEdges(fn func(LoadedImportEdge) error) error {
	if ld == nil {
		return nil
	}
	edges := append([]LoadedImportEdge(nil), ld.ImportEdges...)
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.ParentID != b.ParentID {
			if a.ParentID == "" {
				return true
			}
			if b.ParentID == "" {
				return false
			}
			return a.ParentID < b.ParentID
		}
		if a.ImportID != b.ImportID {
			return a.ImportID < b.ImportID
		}
		if a.ChildID != b.ChildID {
			return a.ChildID < b.ChildID
		}
		return a.DeclaredPath < b.DeclaredPath
	})
	for _, edge := range edges {
		if err := fn(edge); err != nil {
			return err
		}
	}
	return nil
}

type LoadedAssetFile struct {
	Path   string `json:"path"`
	Bytes  []byte `json:"bytes"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type LoadedAsset struct {
	ID           string            `json:"id"`
	DeclaredPath string            `json:"declared_path"`
	IsDir        bool              `json:"is_dir"`
	Files        []LoadedAssetFile `json:"files"`
}
