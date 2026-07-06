package engine

import (
	"context"
	"io"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

type interpreterContext struct {
	def           *ir.LoadedDefinition
	moduleID      string
	wf            *ir.Workflow
	input         map[string]any
	inputFiles    map[string]string
	runEnv        map[string]string // resolved workflow env: name→value (F15); injected into code-step Env, never read from the host here
	runtimeParent string
	runstate      *RunState
	dispatcher    Dispatcher
	resolver      agent.Resolver // exposes adapter lookup for SessionDir (PersistentSession) wiring; nil if dispatcher doesn't implement AdapterResolver
	log           state.Log
	blobs         state.Blobs
	clk           clock.Clock
	tap           io.Writer
	broker        *signal.Broker
	liveFinalizer func(context.Context, LiveDispatchRecord) error
	resume        bool // true when re-entering a folded log (awf resume); gates map-item re-run
}

func (ictx interpreterContext) scope(path string) *Scope {
	return callContextScope(ictx.runstate, ictx.wf, path, ictx.runtimeParent, ictx.input, ictx.inputFiles)
}

// callContextScope builds a *Scope honoring an optional call sub-workflow frame.
// When runtimeParent is non-empty it reads a prefix-stripped child RunState and
// applies the typed-call input/files override, so a called sub-workflow's
// templates resolve against the child's own (unprefixed) view. With an empty
// runtimeParent AND nil input/files it is exactly NewScope(parentRS, wf, path) —
// the top-level behavior. This is the single source of truth for call-aware scope
// construction: both ictx.scope and the reduce executor (engine/reduce.go, via
// newReduceTemplateScopeForExec) go through it, so a reducer resolves refs against
// the SAME frame as the rest of its (possibly called) workflow.
func callContextScope(parentRS *RunState, wf *ir.Workflow, path, runtimeParent string, input map[string]any, inputFiles map[string]string) *Scope {
	if runtimeParent != "" {
		rs := childRunStateForRuntimeParent(parentRS, runtimeParent, input)
		childPath := stripRuntimeParent(path, runtimeParent)
		if input != nil || inputFiles != nil {
			return NewScopeWithInputAndFiles(rs, wf, childPath, input, inputFiles)
		}
		return NewScope(rs, wf, childPath)
	}
	if input != nil || inputFiles != nil {
		return NewScopeWithInputAndFiles(parentRS, wf, path, input, inputFiles)
	}
	return NewScope(parentRS, wf, path)
}

func (ictx interpreterContext) scopeWithVerdict(path string, verdict map[string]any) *Scope {
	if ictx.runtimeParent != "" {
		rs := childRunStateForRuntimeParent(ictx.runstate, ictx.runtimeParent, ictx.input)
		childPath := stripRuntimeParent(path, ictx.runtimeParent)
		if ictx.input != nil || ictx.inputFiles != nil {
			scope := NewScopeWithInputAndFiles(rs, ictx.wf, childPath, ictx.input, ictx.inputFiles)
			scope.verdictOverride = verdict
			return scope
		}
		return NewScopeWithVerdict(rs, ictx.wf, childPath, verdict)
	}
	if ictx.input != nil || ictx.inputFiles != nil {
		scope := NewScopeWithInputAndFiles(ictx.runstate, ictx.wf, path, ictx.input, ictx.inputFiles)
		scope.verdictOverride = verdict
		return scope
	}
	return NewScopeWithVerdict(ictx.runstate, ictx.wf, path, verdict)
}

func childRunStateForRuntimeParent(parent *RunState, runtimeParent string, input map[string]any) *RunState {
	child := NewRunState(parent.RunID, parent.WorkflowDigest, input)
	parent.mu.Lock()
	defer parent.mu.Unlock()
	copyPrefixedCompleted(child.Completed, parent.Completed, runtimeParent)
	copyPrefixedBranches(child.Branches, parent.Branches, runtimeParent)
	copyPrefixedLoopIters(child.LoopIters, parent.LoopIters, runtimeParent)
	copyPrefixedGateAttempts(child.GateAttempts, parent.GateAttempts, runtimeParent)
	copyPrefixedReactRounds(child.ReactRounds, parent.ReactRounds, runtimeParent)
	copyPrefixedMapItems(child.MapItems, parent.MapItems, runtimeParent)
	return child
}

func stripRuntimeParent(path, runtimeParent string) string {
	if path == runtimeParent {
		return ""
	}
	prefix := runtimeParent + "."
	return strings.TrimPrefix(path, prefix)
}
