package engine

import (
	"io"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

type interpreterContext struct {
	def        *ir.LoadedDefinition
	moduleID   string
	wf         *ir.Workflow
	input      map[string]any
	runstate   *RunState
	dispatcher Dispatcher
	log        state.Log
	blobs      state.Blobs
	clk        clock.Clock
	tap        io.Writer
	broker     *signal.Broker
}

func (ictx interpreterContext) scope(path string) *Scope {
	if ictx.input != nil {
		return NewScopeWithInput(ictx.runstate, ictx.wf, path, ictx.input)
	}
	return NewScope(ictx.runstate, ictx.wf, path)
}

func (ictx interpreterContext) scopeWithVerdict(path string, verdict map[string]any) *Scope {
	if ictx.input != nil {
		scope := NewScopeWithInput(ictx.runstate, ictx.wf, path, ictx.input)
		scope.verdictOverride = verdict
		return scope
	}
	return NewScopeWithVerdict(ictx.runstate, ictx.wf, path, verdict)
}
