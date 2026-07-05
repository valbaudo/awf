package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// checkWithConfigForLoadedDefinition is the run-start with:-config guard (U1,
// closes F2). It walks every agent step in the loaded definition and calls the
// resolved adapter's ValidateConfig on the step's with: BEFORE any node
// executes and before the log opens — so a typo'd/missing/wrong-adapter key
// fails fast with ExitUsage (pre-spend) instead of surfacing as a
// permanent_failure mid-run, after earlier steps already ran.
//
// Role resolution mirrors checkCredentialPresenceForLoadedDefinition /
// checkThreadedAdaptersForLoadedDefinition exactly: engine.AgentRuntimeRef
// resolves an `agents:` role name to its registered DerivedAdapter ref, and
// DerivedAdapter.ValidateConfig overlays the role's with: under the step's
// with: before delegating to the base adapter — so a role-supplied required
// key is never false-rejected here. with:-opacity holds: the guard never
// destructures with: beyond reading the single offending key (by name) for
// the template-safety check below.
//
// stderr is accepted but unused — this guard is fatal-only (its error return
// is what the CLI prints); the parameter exists for call-site parity with the
// other run-start guards (checkCredentialPresenceForLoadedDefinition, which
// IS advisory and writes warnings to stderr) that run.go/resume.go invoke
// alongside it in the same guard block.
func checkWithConfigForLoadedDefinition(ld *ir.LoadedDefinition, resolver agent.Resolver, stderr io.Writer) error {
	if ld == nil {
		return nil
	}
	return ld.WalkModules(func(module *ir.LoadedModule) error {
		if module == nil || module.Workflow == nil {
			return nil
		}
		var walkErr error
		ir.WalkNodes(module.Workflow.Graph, "", func(n ir.Node, nodePath string) {
			if walkErr != nil {
				return
			}
			step, ok := n.(*ir.AgentStep)
			if !ok {
				return
			}
			ref := engine.AgentRuntimeRef(module.Workflow, module.ID, step.Uses)
			adapter, ok := resolver.Lookup(ref)
			if !ok {
				return // unresolved ref: resolveRuntimes hard-errors separately
			}
			if err := adapter.ValidateConfig(step.With); err != nil {
				if suppressTemplatedValueErr(err, step.With) {
					return
				}
				walkErr = fmt.Errorf("step %q (uses: %s): %w", nodePath, ref, err)
			}
		})
		return walkErr
	})
}

// suppressTemplatedValueErr returns true for a value-shape error (KeyUnknown
// false) on a key whose value in with: is a templated string ("{{...}}") —
// pre-substitution at run start, so the value cannot yet be validated in its
// final form. Unknown-KEY errors (a typo'd or wrong-adapter key name — never
// fixed by templating) are never suppressed.
func suppressTemplatedValueErr(err error, with ir.RawConfig) bool {
	var ic *agent.ErrInvalidConfig
	if !errors.As(err, &ic) || ic.KeyUnknown || ic.Key == "" {
		return false
	}
	v, ok := with[ic.Key].(string)
	return ok && strings.Contains(v, "{{")
}
