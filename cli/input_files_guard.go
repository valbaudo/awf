package cli

import (
	"fmt"
	"io"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// checkInputFilesForLoadedDefinition is the run-start containerless-
// input_files guard (F31b). A containerless agent step's input_files are
// resolved by the engine and handed to the adapter as inline message parts
// (agent.InputFile) — awf/llm inlines them, but an adapter that does not
// (codex-live) would silently drop the files at Launch. This guard walks
// every agent step in the loaded definition BEFORE any node executes and
// before the log opens, and fails fast with an error naming the offending
// step + adapter instead of letting the run spend money on a step whose
// declared input_files never reach the model.
//
// A step WITH container: is unaffected — container-backed staging (Backend.
// CopyTo before Launch) handles input_files regardless of adapter, so this
// guard only fires on Container == "" (containerless) steps.
//
// stderr is accepted but unused — kept for call-site parity with the other
// run-start guards (checkWithConfigForLoadedDefinition, checkCredentialPresence)
// that run.go/resume.go invoke alongside it in the same guard block.
func checkInputFilesForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil || module.Workflow == nil {
			return nil
		}
		return walkInputFilesNodes(module.Workflow, module.ID, module.Workflow.Graph, resolver)
	})
}

// walkInputFilesNodes recurses exactly like walkCredentialNodes (credential_guard.go);
// keep the switch in sync with that traversal when ir/ adds a new node type.
func walkInputFilesNodes(wf *ir.Workflow, moduleID string, nodes ir.NodeList, resolver agent.Resolver) error {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.Container != "" || len(v.InputFiles) == 0 {
				continue // container-backed staging handles it, or no input_files declared
			}
			ref := engine.AgentRuntimeRef(wf, moduleID, v.Uses)
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				continue // unresolved: resolveRuntimes hard-errors separately
			}
			if !adapter.Capabilities().InlineInputFiles {
				return fmt.Errorf("step %q (uses: %s): input_files on a containerless step requires an adapter that inlines them (awf/llm); %s cannot, so the files would be silently dropped — declare a container: or remove input_files", v.ID, ref, ref)
			}
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip, *ir.React:
			// no agent step; no input_files-inline check needed
		case *ir.If:
			if err := walkInputFilesNodes(wf, moduleID, v.Then, resolver); err != nil {
				return err
			}
			if err := walkInputFilesNodes(wf, moduleID, v.Else, resolver); err != nil {
				return err
			}
		case *ir.Loop:
			if err := walkInputFilesNodes(wf, moduleID, v.Body, resolver); err != nil {
				return err
			}
		case *ir.Try:
			if err := walkInputFilesNodes(wf, moduleID, v.Do, resolver); err != nil {
				return err
			}
			if err := walkInputFilesNodes(wf, moduleID, v.Catch, resolver); err != nil {
				return err
			}
			if err := walkInputFilesNodes(wf, moduleID, v.Finally, resolver); err != nil {
				return err
			}
		case *ir.Parallel:
			if err := walkInputFilesNodes(wf, moduleID, v.Children, resolver); err != nil {
				return err
			}
		case *ir.Gate:
			if err := walkInputFilesNodes(wf, moduleID, v.Generate, resolver); err != nil {
				return err
			}
			if err := walkInputFilesNodes(wf, moduleID, v.Evaluate, resolver); err != nil {
				return err
			}
		case *ir.Map:
			if err := walkInputFilesNodes(wf, moduleID, v.Body, resolver); err != nil {
				return err
			}
		case *ir.Compose:
			if err := walkInputFilesNodes(wf, moduleID, v.Body, resolver); err != nil {
				return err
			}
		default:
			// Unreachable from outside ir/ — ir.Node is a closed sum type.
			// Keep this switch in sync with the runtime-ref traversal (credential_guard.go's).
			panic(fmt.Sprintf("walkInputFilesNodes: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
	return nil
}
