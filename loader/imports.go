package loader

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/valbaudo/awf/ir"
)

const maxImportDepth = 10

var importIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func importRelPath(declared string) (string, error) {
	return safeRootRelPath(declared, safePathPolicy{
		kind:           "import",
		requiredSuffix: ".awf.yaml",
		emptyCode:      "AWF_IMPORT_PATH_INVALID",
		absoluteCode:   "AWF_IMPORT_PATH_ABSOLUTE",
		backslashCode:  "AWF_IMPORT_PATH_BACKSLASH",
		controlCode:    "AWF_IMPORT_PATH_INVALID",
		dotCode:        "AWF_IMPORT_PATH_INVALID",
		escapeCode:     "AWF_IMPORT_PATH_ESCAPE",
		invalidCode:    "AWF_IMPORT_PATH_INVALID",
		suffixCode:     "AWF_IMPORT_PATH_INVALID",
		localizeCode:   "AWF_IMPORT_PATH_INVALID",
	})
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
			code := "AWF_IMPORT_READ"
			message := "read import path"
			if errors.Is(err, errSymlinkComponent) {
				code = "AWF_IMPORT_SYMLINK"
				message = "symlink not permitted in import path"
			}
			return &LoadError{
				Code:    code,
				Source:  parent.WorkflowPath,
				Path:    "imports." + importID,
				Message: message,
				Err:     err,
			}
		}
		childAbs := filepath.Join(filepath.Dir(parent.WorkflowPath), filepath.FromSlash(rel))
		childID := childModuleID(parent.ID, importID)
		childDir := path.Dir(rel)
		childFile := path.Base(rel)
		childRoot, err := root.OpenRoot(childDir)
		if err != nil {
			return &LoadError{
				Code:    "AWF_IMPORT_READ",
				Source:  parent.WorkflowPath,
				Path:    "imports." + importID,
				Message: "open import directory",
				Err:     err,
			}
		}
		*edges = append(*edges, ir.LoadedImportEdge{
			ParentID:     parent.ID,
			ImportID:     importID,
			DeclaredPath: rel,
			ChildID:      childID,
		})
		_, err = loadModuleFromRoot(childRoot, childFile, childAbs, childID, stack, depth+1, modules, edges)
		closeErr := childRoot.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return &LoadError{
				Code:    "AWF_IMPORT_READ",
				Source:  parent.WorkflowPath,
				Path:    "imports." + importID,
				Message: "close import directory",
				Err:     closeErr,
			}
		}
	}
	return nil
}
