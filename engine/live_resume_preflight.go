package engine

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// LiveResumePreflightRequests returns data-only adapter requests for the next
// uncommitted live frontier. It folds over the supplied RunState only; it does
// not append events, dispatch work, or mutate the caller's RunState.
func LiveResumePreflightRequests(ld *ir.LoadedDefinition, rs *RunState, resolver agent.Resolver) ([]agent.LiveResumePreflightRequest, error) {
	if ld == nil || ld.Workflow == nil || rs == nil {
		return nil, nil
	}
	if resolver == nil {
		resolver = &agent.Registry{}
	}
	w := liveResumePreflightWalker{
		ld:       ld,
		resolver: resolver,
	}
	ictx := interpreterContext{
		def:      ld,
		moduleID: "",
		wf:       ld.Workflow,
		runstate: rs,
		input:    rs.Input,
	}
	if _, err := w.walkNodes(ld.Workflow.Graph, "", ictx); err != nil {
		return nil, err
	}
	return w.requests, nil
}

type liveResumePreflightWalker struct {
	ld       *ir.LoadedDefinition
	resolver agent.Resolver
	requests []agent.LiveResumePreflightRequest
}

// walkNodes follows sequential flow and stops at the first uncommitted
// executable frontier. Parallel/map fan-out is handled inside walkNode.
func (w *liveResumePreflightWalker) walkNodes(nodes ir.NodeList, parent string, ictx interpreterContext) (bool, error) {
	for i, n := range nodes {
		found, err := w.walkNode(n, i, parent, ictx)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func (w *liveResumePreflightWalker) walkNode(n ir.Node, idx int, parent string, ictx interpreterContext) (bool, error) {
	switch v := n.(type) {
	case *ir.CodeStep:
		path := ir.PathFor(parent, "", v.ID, idx)
		_, done := ictx.runstate.LookupCompleted(path)
		return !done, nil
	case *ir.SignalStep:
		path := ir.PathFor(parent, "", v.ID, idx)
		_, done := ictx.runstate.LookupCompleted(path)
		return !done, nil
	case *ir.AgentStep:
		return w.walkAgent(v, ir.PathFor(parent, "", v.ID, idx), ictx)
	case *ir.CallStep:
		return w.walkCall(v, ir.PathFor(parent, "", v.ID, idx), ictx)
	case *ir.If:
		return w.walkIf(v, ir.PathFor(parent, "if", "", idx), ictx)
	case *ir.Loop:
		return w.walkLoop(v, ir.PathFor(parent, "loop", "", idx), ictx)
	case *ir.Try:
		return w.walkTry(v, ir.PathFor(parent, "try", "", idx), ictx)
	case *ir.Parallel:
		return w.walkParallel(v, ir.PathFor(parent, "parallel", "", idx), ictx)
	case *ir.Gate:
		return w.walkGate(v, ir.PathFor(parent, "gate", "", idx), ictx)
	case *ir.Map:
		return w.walkMap(v, ir.PathFor(parent, "map", "", idx), ictx)
	case *ir.Compose:
		return w.walkCompose(v, ir.PathFor(parent, "compose", "", idx), ictx)
	case *ir.React:
		// react runs on awf/llm (Containerless+Threaded), never a PersistentSession
		// adapter, so it emits NO live-resume preflight request. But it is still an
		// executable frontier: if uncommitted it is the next thing to run, so — like
		// a CodeStep — it halts the sequential walk (the walk must not preflight
		// nodes downstream of an unfinished react). Keyed on the react[N] node-
		// completion path, mirroring the leaf-step arms above.
		path := ir.PathFor(parent, "react", "", idx)
		_, done := ictx.runstate.LookupCompleted(path)
		return !done, nil
	case *ir.Skip:
		return true, nil
	default:
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: unhandled ir.Node type %T", n)
	}
}

func (w *liveResumePreflightWalker) walkAgent(as *ir.AgentStep, path string, ictx interpreterContext) (bool, error) {
	if _, done := ictx.runstate.LookupCompleted(path); done {
		return false, nil
	}
	ref := AgentRuntimeRef(ictx.wf, ictx.moduleID, as.Uses)
	adapter, ok := w.resolver.Lookup(ref)
	if !ok {
		return false, &agent.ErrAdapterNotFound{Ref: ref}
	}
	if !adapter.Capabilities().PersistentSession {
		return true, nil
	}
	resolvedWith, err := substituteRawConfig(as.With, ictx.scope(path))
	if err != nil {
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: substitute with at %q: %w", path, err)
	}
	w.requests = append(w.requests, agent.LiveResumePreflightRequest{
		NodePath:     path,
		AdapterRef:   ref,
		With:         resolvedWith,
		RunID:        ictx.runstate.RunID,
		CurrentEpoch: ictx.runstate.Epoch,
		NextEpoch:    ictx.runstate.Epoch + 1,
	})
	return true, nil
}

func (w *liveResumePreflightWalker) walkCall(call *ir.CallStep, path string, ictx interpreterContext) (bool, error) {
	if _, done := ictx.runstate.LookupCompleted(path); done {
		return false, nil
	}
	child, ok := callTargetModule(ictx.def, ictx.moduleID, call.Call)
	if !ok || child == nil || child.Workflow == nil {
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: call target %q not found from module %q", call.Call, ictx.moduleID)
	}

	var callInput map[string]any
	if rec, recorded := ictx.runstate.LookupCallStarted(path); recorded {
		callInput = rec.Input
	} else {
		var err error
		callInput, err = evaluateCallInput(call, path, child.Workflow, ictx)
		if err != nil {
			return false, fmt.Errorf("engine.LiveResumePreflightRequests: evaluate call input at %q: %w", path, err)
		}
	}

	runtimeParent := CallWorkflowRuntimePath(path)
	childCtx := ictx
	childCtx.moduleID = child.ID
	childCtx.wf = child.Workflow
	childCtx.input = callInput
	childCtx.runtimeParent = runtimeParent
	found, err := w.walkNodes(child.Workflow.Graph, runtimeParent, childCtx)
	if err != nil || found {
		return found, err
	}
	return true, nil
}

func (w *liveResumePreflightWalker) walkIf(n *ir.If, path string, ictx interpreterContext) (bool, error) {
	which, recorded := ictx.runstate.LookupBranch(path)
	if !recorded {
		cond, err := template.EvalBoolString(string(n.Cond), ictx.scope(path))
		if err != nil {
			return false, fmt.Errorf("engine.LiveResumePreflightRequests: evaluate if cond at %q: %w", path, err)
		}
		if cond {
			which = "then"
		} else {
			which = "else"
		}
	}
	switch which {
	case "then":
		return w.walkNodes(n.Then, path+".then", ictx)
	case "else":
		return w.walkNodes(n.Else, path+".else", ictx)
	default:
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: unknown recorded branch %q at %q", which, path)
	}
}

func (w *liveResumePreflightWalker) walkLoop(n *ir.Loop, path string, ictx interpreterContext) (bool, error) {
	completed := ictx.runstate.LookupLoopIters(path)
	if completed > 0 && n.Until != nil {
		bodyParent := IterPath(path+".body", completed)
		done, err := template.EvalBoolString(string(*n.Until), ictx.scope(bodyParent))
		if err != nil {
			return false, fmt.Errorf("engine.LiveResumePreflightRequests: evaluate loop until at %q: %w", bodyParent, err)
		}
		if done {
			return false, nil
		}
	}
	if n.MaxIters != nil && completed >= *n.MaxIters {
		return false, nil
	}
	iter := completed + 1
	return w.walkNodes(n.Body, IterPath(path+".body", iter), ictx)
}

func (w *liveResumePreflightWalker) walkTry(n *ir.Try, path string, ictx interpreterContext) (bool, error) {
	found, err := w.walkNodes(n.Do, path+".do", ictx)
	if err != nil || found {
		return found, err
	}
	if len(n.Finally) > 0 {
		return w.walkNodes(n.Finally, path+".finally", ictx)
	}
	return false, nil
}

func (w *liveResumePreflightWalker) walkParallel(n *ir.Parallel, path string, ictx interpreterContext) (bool, error) {
	foundAny := false
	for i, child := range n.Children {
		found, err := w.walkNode(child, i, path, ictx)
		if err != nil {
			return false, err
		}
		if found {
			foundAny = true
		}
	}
	return foundAny, nil
}

func (w *liveResumePreflightWalker) walkGate(g *ir.Gate, path string, ictx interpreterContext) (bool, error) {
	attempts := ictx.runstate.LookupGateAttempts(path)
	if len(attempts) > 0 && attempts[len(attempts)-1].AttemptOutcome == AttemptPassed {
		return false, nil
	}
	n := len(attempts) + 1
	if g.MaxAttempts > 0 && n > g.MaxAttempts {
		return true, nil
	}
	attemptPath := AttemptPath(path, n)
	found, err := w.walkNodes(g.Generate, attemptPath+".generate", ictx)
	if err != nil || found {
		return found, err
	}
	found, err = w.walkNodes(g.Evaluate, attemptPath+".evaluate", ictx)
	if err != nil || found {
		return found, err
	}
	return true, nil
}

func (w *liveResumePreflightWalker) walkMap(n *ir.Map, path string, ictx interpreterContext) (bool, error) {
	if _, done := ictx.runstate.LookupCompleted(path); done {
		return false, nil
	}
	overVal, err := evalOver(string(n.Over), ictx.scope(path+".over"))
	if err != nil {
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: evaluate map over at %q: %w", path, err)
	}
	overArr, ok := overVal.([]any)
	if !ok {
		return true, nil
	}
	committed := map[int]string{}
	maxCommittedN := -1
	for _, mr := range ictx.runstate.LookupMapItems(path) {
		committed[mr.N] = mr.Status
		if mr.N > maxCommittedN {
			maxCommittedN = mr.N
		}
	}
	if maxCommittedN >= len(overArr) {
		return false, fmt.Errorf("engine.LiveResumePreflightRequests: map %q: non-deterministic `over` — committed item N=%d exists but current over yields only %d items", path, maxCommittedN, len(overArr))
	}
	statuses := make([]string, len(overArr))
	frontier := false
	for i, item := range overArr {
		if status := committed[i]; status != "" {
			statuses[i] = status
			continue
		}
		frontier = true
		itemCtx := ictx
		itemCtx.runstate = cloneRunStateForPreflight(ictx.runstate)
		itemCtx.runstate.RecordMapItem(path, MapItemRecord{N: i, ItemValue: item})
		if _, err := w.walkNodes(n.Body, ItemPath(path, i), itemCtx); err != nil {
			return false, err
		}
	}
	if frontier {
		return true, nil
	}
	if n.Reduce != nil {
		return true, nil
	}
	pass, _ := tallyResults(statuses)
	pruned := countPruned(statuses)
	effectiveTotal := len(overArr) - pruned
	minSuccess := defaultMinSuccess(n, effectiveTotal)
	if int64(pass) >= minSuccess {
		return false, nil
	}
	return true, nil
}

func (w *liveResumePreflightWalker) walkCompose(n *ir.Compose, path string, ictx interpreterContext) (bool, error) {
	return w.walkNodes(n.Body, path+".body", ictx)
}

func cloneRunStateForPreflight(rs *RunState) *RunState {
	cp := NewRunState(rs.RunID, rs.WorkflowDigest, rs.Input)
	cp.Epoch = rs.Epoch
	cp.Assets = rs.Assets
	cp.Paused = rs.Paused
	cp.Cancelled = rs.Cancelled
	cp.CancelReason = rs.CancelReason
	cp.Completed = copyCompletedMap(rs.Completed)
	cp.Branches = copyStringMap(rs.Branches)
	cp.LoopIters = copyIntMap(rs.LoopIters)
	cp.GateAttempts = copyGateAttemptsMap(rs.GateAttempts)
	cp.MapItems = copyMapItemsMap(rs.MapItems)
	cp.Signals = copySignalsMap(rs.Signals)
	cp.CallStarted = copyCallStartedMap(rs.CallStarted)
	cp.SignalReceivedAt = copySignalReceivedAtMap(rs.SignalReceivedAt)
	cp.SnapshotRefs = copyStringMap(rs.SnapshotRefs)
	return cp
}

func copyCompletedMap(in map[string]NodeResult) map[string]NodeResult {
	out := make(map[string]NodeResult, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyGateAttemptsMap(in map[string][]AttemptResult) map[string][]AttemptResult {
	out := make(map[string][]AttemptResult, len(in))
	for k, v := range in {
		out[k] = append([]AttemptResult(nil), v...)
	}
	return out
}

func copyMapItemsMap(in map[string][]MapItemRecord) map[string][]MapItemRecord {
	out := make(map[string][]MapItemRecord, len(in))
	for k, v := range in {
		out[k] = append([]MapItemRecord(nil), v...)
	}
	return out
}

func copySignalsMap(in map[string][]SignalEntry) map[string][]SignalEntry {
	out := make(map[string][]SignalEntry, len(in))
	for k, v := range in {
		out[k] = append([]SignalEntry(nil), v...)
	}
	return out
}

func copyCallStartedMap(in map[string]CallStartedRecord) map[string]CallStartedRecord {
	out := make(map[string]CallStartedRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copySignalReceivedAtMap(in map[string]SignalReceivedEntry) map[string]SignalReceivedEntry {
	out := make(map[string]SignalReceivedEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
