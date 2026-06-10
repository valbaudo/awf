package engine

import (
	"io"
	"strings"

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
	runtimeParent string
	runstate      *RunState
	dispatcher    Dispatcher
	log           state.Log
	blobs         state.Blobs
	clk           clock.Clock
	tap           io.Writer
	broker        *signal.Broker
}

func (ictx interpreterContext) scope(path string) *Scope {
	if ictx.runtimeParent != "" {
		rs := childRunStateForRuntimeParent(ictx.runstate, ictx.runtimeParent, ictx.input)
		childPath := stripRuntimeParent(path, ictx.runtimeParent)
		if ictx.input != nil {
			return NewScopeWithInput(rs, ictx.wf, childPath, ictx.input)
		}
		return NewScope(rs, ictx.wf, childPath)
	}
	if ictx.input != nil {
		return NewScopeWithInput(ictx.runstate, ictx.wf, path, ictx.input)
	}
	return NewScope(ictx.runstate, ictx.wf, path)
}

func (ictx interpreterContext) scopeWithVerdict(path string, verdict map[string]any) *Scope {
	if ictx.runtimeParent != "" {
		rs := childRunStateForRuntimeParent(ictx.runstate, ictx.runtimeParent, ictx.input)
		childPath := stripRuntimeParent(path, ictx.runtimeParent)
		if ictx.input != nil {
			scope := NewScopeWithInput(rs, ictx.wf, childPath, ictx.input)
			scope.verdictOverride = verdict
			return scope
		}
		return NewScopeWithVerdict(rs, ictx.wf, childPath, verdict)
	}
	if ictx.input != nil {
		scope := NewScopeWithInput(ictx.runstate, ictx.wf, path, ictx.input)
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
