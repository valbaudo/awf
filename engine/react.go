package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// defaultMaxTurns is the react: loop budget when max_turns is unset (spec §5).
const defaultMaxTurns = 8

// toolResultCap bounds the model-facing tool result (spec §4.5). The full output
// stays on the committed .tool-J leaf; only what the NEXT model call sees is
// capped. Un-configurable in v1 (matches the fixed tool_choice/max_turns).
const toolResultCap = 16384

// runReactWithContext is the *ir.React handler — a model+tools loop on the
// awf/llm path with exact resume (spec §4). NOTE (rev #1): there is NO `resolver`
// field on interpreterContext (it carries only `dispatcher Dispatcher`); the
// runner is extracted from the *LocalDispatcher inside toolLoopRunnerFor.
func runReactWithContext(ctx context.Context, r *ir.React, reactPath string, ictx interpreterContext) (Outcome, error) {
	// Terminal short-circuit (reduce.go:150 / spec §4.2): a committed react node
	// replays — the synthetic .model leaf does NOT inherit the code-step
	// short-circuit, so this entry guard + the per-round model-leaf guard are the
	// resume invariant.
	if _, done := ictx.runstate.LookupCompleted(reactPath); done {
		return OutcomeOK, nil
	}

	runner, err := toolLoopRunnerFor(r, ictx)
	if err != nil {
		return "", err
	}

	appendNodeStarted(ictx.log, reactPath, "react") // obs span-open parity (rev #19)

	maxTurns := r.MaxTurns
	if maxTurns == 0 {
		maxTurns = defaultMaxTurns
	}

	startK := len(ictx.runstate.LookupReactRounds(reactPath)) + 1

	// Rebuild the conversation from committed rounds 1..startK-1 (no re-sample, no
	// re-dispatch) and prepend the initial user turn.
	msgs, err := replayMessages(r, reactPath, ictx, startK)
	if err != nil {
		return "", err
	}

	for k := startK; k <= maxTurns; k++ {
		roundPath := RoundPath(reactPath, k)
		modelPath := ModelPath(roundPath)

		// 1. Model leaf, guarded (spec §4.3 execute step 1). A committed leaf is
		//    read back; the non-deterministic model is never re-sampled.
		var mr modelResult
		if nr, ok := ictx.runstate.LookupCompleted(modelPath); ok {
			mr, err = roundResultFromLeaf(nr)
			if err != nil {
				return "", fmt.Errorf("engine.runReact: read model leaf %q: %w", modelPath, err)
			}
		} else {
			res, rerr := runner.RunToolLoop(ctx, buildToolLoopInvocation(r, ictx.wf, modelPath, msgs))
			if rerr != nil {
				// A parse miss on a natural-stop schema round is the model-call
				// step's retryable failure (rev #4 / §5), NOT an internal halt.
				var pe *agent.ErrUnparseableOutput
				if errors.As(rerr, &pe) {
					return failStep(ictx.log, reactPath, OutcomeRetryableFailure, rerr)
				}
				return "", fmt.Errorf("engine.runReact: model call at %q: %w", modelPath, rerr)
			}
			mr = toModelResult(res)
			if err := commitModelLeaf(ictx, modelPath, mr); err != nil {
				return "", err
			}
		}
		msgs = append(msgs, buildAssistantTurn(mr))

		// 2. Terminate decision — BEFORE dispatching any tool (spec §4.3 step 2).
		if mr.FinishReason != "tool_calls" {
			return commitTerminal(ictx, r, reactPath, mr, "stop")
		}
		if k == maxTurns {
			// max_turns: stop WITHOUT dispatching the dangling tools (spec §5).
			return commitTerminal(ictx, r, reactPath, mr, "max_turns")
		}

		// 3. Dispatch tools (in stored Index order).
		if err := dispatchRoundTools(ctx, r, roundPath, mr, &msgs, ictx); err != nil {
			return "", err
		}

		// 4. Close the round (Append+Sync the marker AFTER the model leaf + every
		//    dispatched tool leaf committed — crash≠verdict ordering, spec §4.4).
		if err := closeRound(ictx, reactPath, k); err != nil {
			return "", err
		}
	}

	// Unreachable: the k==maxTurns branch returns inside the loop.
	return "", fmt.Errorf("engine.runReact: %q fell through the round loop (maxTurns=%d)", reactPath, maxTurns)
}

// modelResult is the engine-private mirror of agent.ToolLoopResult, carrying the
// fields runReact needs to commit/replay a round and make the stop decision.
type modelResult struct {
	Text         string
	Output       map[string]any
	ToolCalls    []agent.ToolCall
	FinishReason string
}

func toModelResult(res agent.ToolLoopResult) modelResult {
	return modelResult{
		Text:         res.Text,
		Output:       res.Output,
		ToolCalls:    res.ToolCalls,
		FinishReason: res.FinishReason,
	}
}

// toolLoopRunnerFor resolves the react node's awf/llm adapter to an
// agent.ToolLoopRunner via the *LocalDispatcher's Resolver (the call_step.go /
// compose.go precedent — there is NO phantom interpreterContext.resolver field).
// Gated on Caps.Containerless && Caps.Threaded; the interface assertion works
// through *agent.DerivedAdapter (rev #1 / C2).
func toolLoopRunnerFor(r *ir.React, ictx interpreterContext) (agent.ToolLoopRunner, error) {
	ld, ok := ictx.dispatcher.(*LocalDispatcher)
	if !ok {
		return nil, fmt.Errorf("engine.runReact: react: requires the local dispatcher (got %T)", ictx.dispatcher)
	}
	if ld.Resolver == nil {
		return nil, fmt.Errorf("engine.runReact: react: dispatcher has no agent resolver")
	}
	uses, _ := r.With["uses"].(string)
	adapter, ok := ld.Resolver.Lookup(uses)
	if !ok {
		return nil, fmt.Errorf("engine.runReact: no adapter %q", uses)
	}
	caps := adapter.Capabilities()
	if !caps.Containerless || !caps.Threaded {
		return nil, fmt.Errorf("engine.runReact: adapter %q is not a containerless+threaded adapter", uses)
	}
	runner, ok := adapter.(agent.ToolLoopRunner)
	if !ok {
		return nil, fmt.Errorf("engine.runReact: adapter %q does not implement agent.ToolLoopRunner", uses)
	}
	return runner, nil
}

// buildToolLoopInvocation assembles one model call: the prior message history,
// the selected tools (from wf.Tools), the output schema, and the .model leaf path.
func buildToolLoopInvocation(r *ir.React, wf *ir.Workflow, modelPath string, msgs []agent.ReactTurn) agent.ToolLoopInvocation {
	tools := make([]agent.ToolDef, 0, len(r.Tools))
	for _, name := range r.Tools {
		t, ok := wf.Tools[name]
		if !ok {
			continue // validator AWF1053 rejects unknown tool names; defensive skip
		}
		var schema map[string]any
		if t.InputSchema != nil {
			schema = map[string]any(*t.InputSchema)
		}
		tools = append(tools, agent.ToolDef{
			Name:        name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	uses, _ := r.With["uses"].(string)
	return agent.ToolLoopInvocation{
		NodePath:     modelPath,
		Uses:         uses,
		With:         r.With,
		Messages:     msgs,
		Tools:        tools,
		OutputSchema: r.OutputSchema,
	}
}

// buildAssistantTurn builds the assistant message for the messages array from a
// model round result. Used on BOTH the fresh-commit path and replay (one
// construction path → no fresh-vs-resume drift). OMITS tool_calls entirely when
// empty — an empty tool_calls:[] on the wire is a 400 (rev M5 / §6).
func buildAssistantTurn(mr modelResult) agent.ReactTurn {
	turn := agent.ReactTurn{Role: "assistant", Content: mr.Text}
	if len(mr.ToolCalls) > 0 {
		turn.ToolCalls = mr.ToolCalls
	}
	return turn
}

// commitModelLeaf commits the synthetic .model leaf carrying {text,
// finish_reason, tool_calls, output?} (reduce-style). tool_calls store each call
// VERBATIM (index, id, name, arguments-as-raw-string) — the §4.5 determinism
// invariant: a Go string round-trips byte-identically through Blobs/Fold.
func commitModelLeaf(ictx interpreterContext, modelPath string, mr modelResult) error {
	out := modelLeafOutputs(mr)
	nr, err := Commit(ictx.log, ictx.blobs, modelPath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)
	if err != nil {
		return fmt.Errorf("engine.runReact: commit model leaf at %q: %w", modelPath, err)
	}
	ictx.runstate.RecordCompleted(modelPath, nr)
	return nil
}

// modelLeafOutputs is the canonical map shape stored on a .model leaf and read
// back by roundResultFromLeaf. Kept in one place so the round-trip stays stable.
func modelLeafOutputs(mr modelResult) map[string]any {
	calls := make([]any, 0, len(mr.ToolCalls))
	for _, tc := range mr.ToolCalls {
		calls = append(calls, map[string]any{
			"index":     tc.Index,
			"id":        tc.ID,
			"name":      tc.Name,
			"arguments": tc.Arguments, // verbatim raw string (§4.5)
		})
	}
	out := map[string]any{
		"text":          mr.Text,
		"finish_reason": mr.FinishReason,
		"tool_calls":    calls,
	}
	if mr.Output != nil {
		out["output"] = mr.Output
	}
	return out
}

// roundResultFromLeaf reconstructs a modelResult from a committed .model leaf's
// Outputs (folded back as map[string]any). The verbatim-args invariant rides
// here: each tool_call's arguments is the Go string stored on the leaf,
// round-tripped byte-identically (a nested map would not).
func roundResultFromLeaf(nr NodeResult) (modelResult, error) {
	mr := modelResult{}
	if s, ok := nr.Outputs["text"].(string); ok {
		mr.Text = s
	}
	if s, ok := nr.Outputs["finish_reason"].(string); ok {
		mr.FinishReason = s
	}
	if o, ok := nr.Outputs["output"].(map[string]any); ok {
		mr.Output = o
	}
	raw, ok := nr.Outputs["tool_calls"].([]any)
	if !ok {
		return mr, nil // no tool calls (natural-stop round)
	}
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return modelResult{}, fmt.Errorf("tool_calls[%d] is %T, want object", i, e)
		}
		mr.ToolCalls = append(mr.ToolCalls, agent.ToolCall{
			Index:     jsonInt(m["index"]),
			ID:        asString(m["id"]),
			Name:      asString(m["name"]),
			Arguments: asString(m["arguments"]),
		})
	}
	return mr, nil
}

// replayMessages rebuilds the []ReactTurn for rounds 1..startK-1 purely from
// committed .model + .tool-J leaves (no model call, no tool dispatch) and
// prepends the initial user turn (spec §4.3 replay). The tool-message ToolCallID
// is read from the same stored ids so assistant.tool_calls[*].id == tool.id.
func replayMessages(r *ir.React, reactPath string, ictx interpreterContext, startK int) ([]agent.ReactTurn, error) {
	msgs := []agent.ReactTurn{{Role: "user", Content: r.Prompt}}
	for k := 1; k < startK; k++ {
		roundPath := RoundPath(reactPath, k)
		modelPath := ModelPath(roundPath)
		nr, ok := ictx.runstate.LookupCompleted(modelPath)
		if !ok {
			return nil, fmt.Errorf("engine.runReact: replay missing committed model leaf at %q", modelPath)
		}
		mr, err := roundResultFromLeaf(nr)
		if err != nil {
			return nil, fmt.Errorf("engine.runReact: replay read model leaf %q: %w", modelPath, err)
		}
		msgs = append(msgs, buildAssistantTurn(mr))
		for _, tc := range mr.ToolCalls {
			toolPath := ToolPath(roundPath, tc.Index)
			tnr, ok := ictx.runstate.LookupCompleted(toolPath)
			if !ok {
				return nil, fmt.Errorf("engine.runReact: replay missing committed tool leaf at %q", toolPath)
			}
			msgs = append(msgs, toolMessageFromLeaf(tc.ID, tnr))
		}
	}
	return msgs, nil
}

// toolMessageFromLeaf rebuilds a tool-role ReactTurn from a committed .tool-J
// leaf. The model-facing content was bounded at commit time and stored verbatim
// in the leaf's outputs as "content"; ToolCallID matches the triggering call.
func toolMessageFromLeaf(toolCallID string, nr NodeResult) agent.ReactTurn {
	content := asString(nr.Outputs["content"])
	return agent.ReactTurn{Role: "tool", ToolCallID: toolCallID, Content: content}
}

// dispatchRoundTools dispatches every tool_call in stored Index order. A
// committed leaf is replayed; an unknown tool name feeds an error tool message
// (no dispatch); otherwise the impl is synthesized + dispatched (dispatchOneTool).
func dispatchRoundTools(ctx context.Context, r *ir.React, roundPath string, mr modelResult, msgs *[]agent.ReactTurn, ictx interpreterContext) error {
	for _, tc := range mr.ToolCalls {
		toolPath := ToolPath(roundPath, tc.Index)
		if nr, ok := ictx.runstate.LookupCompleted(toolPath); ok {
			*msgs = append(*msgs, toolMessageFromLeaf(tc.ID, nr))
			continue
		}
		tool, ok := toolByName(r, ictx.wf, tc.Name)
		if !ok {
			*msgs = append(*msgs, agent.ReactTurn{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    fmt.Sprintf("error: unknown tool %q", tc.Name),
			})
			continue
		}
		content, err := dispatchOneTool(ctx, tool, toolPath, tc, ictx)
		if err != nil {
			return err // genuine infra/dispatch failure — hard react-step failure
		}
		*msgs = append(*msgs, agent.ReactTurn{Role: "tool", ToolCallID: tc.ID, Content: content})
	}
	return nil
}

// toolByName returns the workflow tool for a call name, but only when the react
// node actually offers it (tc.Name must be in r.Tools — defends the unknown-tool
// path even though the model selected it).
func toolByName(r *ir.React, wf *ir.Workflow, name string) (ir.Tool, bool) {
	offered := false
	for _, n := range r.Tools {
		if n == name {
			offered = true
			break
		}
	}
	if !offered {
		return ir.Tool{}, false
	}
	t, ok := wf.Tools[name]
	return t, ok
}

// dispatchOneTool synthesizes a CodeStep from the tool's ToolImpl, stages the
// VERBATIM arguments bytes as a per-call container InputFile, dispatches with
// Attempts:1 (a deterministic tool failure is fed back to the model after ONE
// attempt, not re-run 3×), and commits the .tool-J leaf. A plain non-zero exit
// (dr.ExitCode != nil) is rewritten to an OK leaf carrying {exit_code, stdout,
// content} and fed back; an infra failure (dr.ExitCode == nil) is a hard error.
// Returns the model-facing (bounded) tool result content.
func dispatchOneTool(ctx context.Context, tool ir.Tool, toolPath string, tc agent.ToolCall, ictx interpreterContext) (string, error) {
	argsPath := argsFilePath(toolPath)
	scope := newToolImplScope(ictx.runstate, ictx.wf, toolPath, argsPath, parseArgsBestEffort(tc.Arguments))
	cmd, err := template.Substitute(tool.Impl.Run, scope)
	if err != nil {
		// Author's templating bug in the impl run: — hard react-step failure
		// (it would fail identically on retry; the model can't fix the config).
		return "", fmt.Errorf("engine.runReact: template tool impl at %q: %w", toolPath, err)
	}

	policy := retry.Policy{Attempts: 1}
	if tool.Impl.Retry != nil {
		merged, merr := retry.Merge(retry.Default, tool.Impl.Retry)
		if merr != nil {
			return "", fmt.Errorf("engine.runReact: build tool retry policy at %q: %w", toolPath, merr)
		}
		policy = merged
	}

	outputFiles, ferr := toolOutputFilePaths(tool.Impl.OutputFiles, scope)
	if ferr != nil {
		return "", fmt.Errorf("engine.runReact: template tool output_files at %q: %w", toolPath, ferr)
	}
	synth := &ir.CodeStep{Run: cmd, Container: tool.Impl.Container, OutputFiles: tool.Impl.OutputFiles}
	resolved := ResolvedInputs{
		Command:     cmd,
		Env:         map[string]string{},
		InputFiles:  []container.InputFile{{Path: argsPath, Content: []byte(tc.Arguments)}}, // verbatim
		OutputFiles: outputFiles,
	}
	if tool.Impl.Timeout != nil {
		resolved.Timeout = time.Duration(*tool.Impl.Timeout)
	}
	intent := NodeIntent{Path: toolPath, Node: synth, ResolvedInputs: resolved}

	dr, chunks, runErr := RunWithRetry(ctx, ictx.dispatcher, intent, policy, ictx.clk, ictx.log)
	drainTap(chunks, "react.tool", ictx.tap)

	if dr.ExitCode == nil {
		// The backend never produced an exit code — an infra/dispatch failure
		// (couldn't exec the container), distinct from the tool running and
		// exiting non-zero. Hard react-step failure (after the impl's own retry).
		if runErr == nil {
			runErr = fmt.Errorf("tool impl produced no exit code (no underlying error)")
		}
		return "", fmt.Errorf("engine.runReact: dispatch tool impl at %q: %w", toolPath, runErr)
	}

	// The process ran (possibly non-zero). Rewrite to an OK leaf carrying the
	// result and feed the bounded output back to the model — the react step does
	// NOT fail on a tool's own non-zero exit (spec §4.5).
	exec := container.ExecResult{ExitCode: *dr.ExitCode, Stdout: dr.Stdout}
	content := boundToolResult(exec)

	out := map[string]any{
		"exit_code": *dr.ExitCode,
		"stdout":    string(dr.Stdout),
		"content":   content, // the bounded, model-facing message (read back on replay)
	}
	leafDR := DispatchResult{
		Outcome:  OutcomeOK,
		ExitCode: dr.ExitCode,
		Outputs:  out,
		Stdout:   dr.Stdout,
		Files:    dr.Files, // declared output_files captured to the leaf (not surfaced; §4.5)
	}
	nr, commitErr := Commit(ictx.log, ictx.blobs, toolPath, leafDR, false)
	if commitErr != nil {
		return "", fmt.Errorf("engine.runReact: commit tool leaf at %q: %w", toolPath, commitErr)
	}
	ictx.runstate.RecordCompleted(toolPath, nr)
	return content, nil
}

// closeRound appends the react.round marker (Append+Sync, gate-style — a
// deliberate Sync unlike loop.iter, because re-running tool side-effects is not
// first-run-equivalent, spec §4.1) and records it in RunState. Marker event Path
// is the react node path R (NOT the round path), mirroring gate.attempt/loop.iter
// so Fold keys ReactRounds[R] and LookupReactRounds(R) matches.
func closeRound(ictx interpreterContext, reactPath string, k int) error {
	data, err := json.Marshal(ReactRoundData{N: k})
	if err != nil {
		return fmt.Errorf("engine.runReact: marshal react.round at %q: %w", reactPath, err)
	}
	if err := ictx.log.Append(state.Event{Type: EventReactRound, Path: reactPath, Data: data}); err != nil {
		return fmt.Errorf("engine.runReact: append react.round at %q: %w", reactPath, err)
	}
	if err := ictx.log.Sync(); err != nil {
		return fmt.Errorf("engine.runReact: sync react.round at %q: %w", reactPath, err)
	}
	ictx.runstate.RecordReactRound(reactPath, ReactRoundRecord{N: k})
	return nil
}

// commitTerminal commits the terminal node.completed at R (spec §5). The output
// always carries the reserved stop_reason sibling. On natural stop with an
// output_schema the engine VALIDATES the adapter-parsed mr.Output with
// ValidateOutputMap (parsing is adapter-side — there is no parseTypedOutput); a
// miss is OutcomeRetryableFailure. On max_turns the schema is NOT enforced and
// the only data field is text.
func commitTerminal(ictx interpreterContext, r *ir.React, reactPath string, mr modelResult, stopReason string) (Outcome, error) {
	out := map[string]any{}
	switch {
	case stopReason == "max_turns":
		out["text"] = mr.Text // schema NOT enforced on truncation (§5)
	case r.OutputSchema != nil:
		if err := ValidateOutputMap(mr.Output, r.OutputSchema); err != nil {
			return failStep(ictx.log, reactPath, OutcomeRetryableFailure, err)
		}
		for k, v := range mr.Output {
			out[k] = v
		}
	default:
		out["text"] = mr.Text
	}
	out["stop_reason"] = stopReason // reserved sibling (validate forbids a clashing schema field)

	nr, err := Commit(ictx.log, ictx.blobs, reactPath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)
	if err != nil {
		return "", fmt.Errorf("engine.runReact: commit terminal at %q: %w", reactPath, err)
	}
	ictx.runstate.RecordCompleted(reactPath, nr)
	return OutcomeOK, nil
}

// boundToolResult bounds the model-facing tool result (spec §4.5). Non-UTF-8
// stdout → a size+exit descriptor (json.Marshal silently maps invalid UTF-8 to
// U+FFFD, so inlining binary feeds corruption, not an error; ExecResult carries
// no Files, so there is no artifact ref to give). Valid UTF-8 → the existing
// same-package boundDisplayField (16384-byte cap, UTF-8 rune-boundary backup +
// "...[truncated N bytes]" marker) with an [exit E] prefix on non-zero. The full
// output stays on the committed .tool-J leaf.
func boundToolResult(res container.ExecResult) string {
	body := res.Stdout
	if !utf8.Valid(body) {
		return fmt.Sprintf("[non-text tool output: %d bytes, exit %d]", len(body), res.ExitCode)
	}
	s := boundDisplayField(string(body), toolResultCap)
	if res.ExitCode != 0 {
		s = fmt.Sprintf("[exit %d]\n%s", res.ExitCode, s)
	}
	return s
}

// argsFilePath derives a per-call container path for the staged verbatim
// arguments from the tool leaf path (resume-stable: same toolPath → same file).
func argsFilePath(toolPath string) string {
	return "/work/.awf/" + sanitizePathSegment(toolPath) + ".args.json"
}

// sanitizePathSegment turns a runtime path (containing '.', '[', ']') into a
// single safe filename segment.
func sanitizePathSegment(p string) string {
	b := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '.', '[', ']', '/', ' ':
			b = append(b, '_')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// parseArgsBestEffort unmarshals raw model-emitted arguments into a map for the
// {{ args.<field> }} binding. A parse miss → nil (the scalars are simply absent;
// {{ args_file }} still carries the verbatim bytes — spec §4.5).
func parseArgsBestEffort(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// toolOutputFilePaths templates the container paths a tool impl declares to
// capture, through the tool-impl scope (so {{ args.* }}/{{ args_file }} resolve
// in an output_files path too). The captured files land on the .tool-J leaf but
// are NOT surfaced to the model in v1 (spec §4.5).
func toolOutputFilePaths(ofs ir.OutputFiles, scope template.Scope) ([]string, error) {
	if len(ofs) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, len(ofs))
	for _, of := range ofs {
		p, err := template.Substitute(of.Path, scope)
		if err != nil {
			return nil, fmt.Errorf("output_files %q: %w", of.Name, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// jsonInt reads an int that may have round-tripped through JSON (float64) or be
// a native Go int (fresh-commit path).
func jsonInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
