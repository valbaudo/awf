package loader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/valbaudo/awf/ir"
)

const maxImportDepth = 10

var importIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func importRelPath(declared string) (string, error) {
	return safeRootRelPath(declared, safePathPolicy{kind: "import", requiredSuffix: ".awf.yaml"})
}

func validImportID(id string) bool {
	return importIDPattern.MatchString(id) && !strings.Contains(id, ".") && id != ir.CallWorkflowSegment
}

func childModuleID(parentID, importID string) string {
	if parentID == "" {
		return importID
	}
	return parentID + "." + importID
}

func loadImports(
	parent *ir.LoadedModule,
	root *os.Root,
	stack []string,
	depth int,
	modules map[string]*ir.LoadedModule,
	edges *[]ir.LoadedImportEdge,
) error {
	ids := make([]string, 0, len(parent.Workflow.Imports))
	for id := range parent.Workflow.Imports {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, importID := range ids {
		declared := parent.Workflow.Imports[importID]
		if !validImportID(importID) {
			return &LoadError{
				Code:    "AWF_IMPORT_ID_INVALID",
				Source:  parent.WorkflowPath,
				Path:    "imports." + importID,
				Message: fmt.Sprintf("invalid import id %q", importID),
			}
		}
		rel, err := importRelPath(declared)
		if err != nil {
			var pathErr *safePathError
			if errors.As(err, &pathErr) {
				return &LoadError{
					Code:    pathErr.Code,
					Source:  parent.WorkflowPath,
					Path:    "imports." + importID,
					Message: pathErr.Message,
					Err:     err,
				}
			}
			return err
		}
		if err := rejectSymlinkComponents(root, rel); err != nil {
			return &LoadError{
				Code:    "AWF_IMPORT_SYMLINK",
				Source:  parent.WorkflowPath,
				Path:    "imports." + importID,
				Message: "symlink not permitted in import path",
				Err:     err,
			}
		}
		childAbs := filepath.Join(filepath.Dir(parent.WorkflowPath), filepath.FromSlash(rel))
		childID := childModuleID(parent.ID, importID)
		*edges = append(*edges, ir.LoadedImportEdge{
			ParentID:     parent.ID,
			ImportID:     importID,
			DeclaredPath: rel,
			ChildID:      childID,
		})
		if _, err := loadModule(childAbs, childID, stack, depth+1, modules, edges); err != nil {
			return err
		}
	}
	return nil
}
