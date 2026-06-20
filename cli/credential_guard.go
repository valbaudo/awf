package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// checkCredentialPresence is the advisory credential-presence preflight.
// It walks every agent step in the loaded definition; for each step whose
// resolved adapter implements agent.CredentialNamer, it checks whether at
// least one of RequiredEnv() is present in the host env (os.LookupEnv).
//
// WARN-IF-NONE semantics: if NONE of the adapter's credential vars is set,
// a warning line is printed to stderr naming the adapter ref and the missing
// vars. At least one present → no warning (provider-alternative adapters
// should not false-warn when only one key is configured).
//
// Dedup: each adapter ref warns at most once (same adapter, many steps →
// one warning). Returns nil always — this is advisory; it never fails a run.
func checkCredentialPresence(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	return checkCredentialPresenceForLoadedDefinition(ld, resolver, stderr)
}

func checkCredentialPresenceForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	if ld == nil {
		return nil
	}
	warned := map[string]bool{} // dedup per adapter ref
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil {
			return nil
		}
		if module.Workflow == nil {
			return nil
		}
		walkCredentialNodes(module.Workflow, module.ID, module.Workflow.Graph, resolver, warned, stderr)
		return nil
	})
}

func walkCredentialNodes(wf *ir.Workflow, moduleID string, nodes ir.NodeList, resolver agent.Resolver, warned map[string]bool, stderr io.Writer) {
	for _, n := range nodes {
		switch v := n.(type) {
		case *ir.AgentStep:
			ref := engine.AgentRuntimeRef(wf, moduleID, v.Uses)
			if warned[ref] {
				continue
			}
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				continue // unresolved adapter — resolveRuntimes will hard-error; skip silently
			}
			cn, ok := adapter.(agent.CredentialNamer)
			if !ok {
				continue // adapter does not declare credential requirements
			}
			envs := cn.RequiredEnv()
			if credentialPresent(envs) {
				continue // at least one credential env is set — OK
			}
			warned[ref] = true
			_, _ = fmt.Fprintf(stderr, "Warning: agent step %q: adapter %q — none of its credential env vars %v is set; the run will likely fail at Launch\n", v.ID, ref, envs)
		case *ir.CodeStep, *ir.SignalStep, *ir.CallStep, *ir.Skip, *ir.React:
			// no agent step; no credential check needed
		case *ir.If:
			walkCredentialNodes(wf, moduleID, v.Then, resolver, warned, stderr)
			walkCredentialNodes(wf, moduleID, v.Else, resolver, warned, stderr)
		case *ir.Loop:
			walkCredentialNodes(wf, moduleID, v.Body, resolver, warned, stderr)
		case *ir.Try:
			walkCredentialNodes(wf, moduleID, v.Do, resolver, warned, stderr)
			walkCredentialNodes(wf, moduleID, v.Catch, resolver, warned, stderr)
			walkCredentialNodes(wf, moduleID, v.Finally, resolver, warned, stderr)
		case *ir.Parallel:
			walkCredentialNodes(wf, moduleID, v.Children, resolver, warned, stderr)
		case *ir.Gate:
			walkCredentialNodes(wf, moduleID, v.Generate, resolver, warned, stderr)
			walkCredentialNodes(wf, moduleID, v.Evaluate, resolver, warned, stderr)
		case *ir.Map:
			walkCredentialNodes(wf, moduleID, v.Body, resolver, warned, stderr)
		case *ir.Compose:
			walkCredentialNodes(wf, moduleID, v.Body, resolver, warned, stderr)
		default:
			// Unreachable from outside ir/ — ir.Node is a closed sum type.
			// Keep this switch in sync with threaded_guard.go's traversal.
			panic(fmt.Sprintf("walkCredentialNodes: unhandled ir.Node type %T (extend the switch; mirror the runtime-ref traversal)", n))
		}
	}
}

// credentialPresent returns true if at least one of the given env var names
// is set (non-empty) in the host environment.
func credentialPresent(envs []string) bool {
	for _, e := range envs {
		if v, ok := os.LookupEnv(e); ok && v != "" {
			return true
		}
	}
	return false
}
