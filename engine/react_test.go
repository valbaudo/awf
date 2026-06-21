package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// ---------------------------------------------------------------------------
// scriptedToolLoop — a fake agent.Adapter + agent.ToolLoopRunner that returns a
// pre-programmed agent.ToolLoopResult per call index. Records each invocation so
// tests can assert on the message history (tool_call_id equality, replay). Lives
// in the test (Phase 4 must not touch agent/fake — that's Phase 5).
// ---------------------------------------------------------------------------

type scriptedToolLoop struct {
	results []agent.ToolLoopResult
	errs    []error // optional per-call error; nil entries → no error
	calls   []agent.ToolLoopInvocation
}

const reactAdapterRef = "awf/llm"

func (s *scriptedToolLoop) Ref() string { return reactAdapterRef }
func (s *scriptedToolLoop) Capabilities() agent.Caps {
	return agent.Caps{Containerless: true, Threaded: true}
}
func (s *scriptedToolLoop) Version(context.Context, container.Handle) (string, error) {
	return "fake-0", nil
}
func (s *scriptedToolLoop) ValidateConfig(ir.RawConfig) error { return nil }
func (s *scriptedToolLoop) Launch(context.Context, container.Handle, agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	return nil, nil, fmt.Errorf("scriptedToolLoop.Launch: not used by react: (RunToolLoop only)")
}

func (s *scriptedToolLoop) RunToolLoop(_ context.Context, inv agent.ToolLoopInvocation) (agent.ToolLoopResult, error) {
	i := len(s.calls)
	s.calls = append(s.calls, inv)
	if i >= len(s.results) {
		return agent.ToolLoopResult{}, fmt.Errorf("scriptedToolLoop: no script entry for call %d (over-sampled — resume re-sampled a committed round?)", i)
	}
	if i < len(s.errs) && s.errs[i] != nil {
		return s.results[i], s.errs[i]
	}
	return s.results[i], nil
}

func (s *scriptedToolLoop) Calls() []agent.ToolLoopInvocation { return s.calls }

var (
	_ agent.Adapter        = (*scriptedToolLoop)(nil)
	_ agent.ToolLoopRunner = (*scriptedToolLoop)(nil)
)

// ---------------------------------------------------------------------------
// reactTestHarness — wires a RunState, in-mem log+blobs, a fake container
// backend (for tool impls), and an agent.Registry holding the scripted runner,
// then drives runReactWithContext at react[0].
// ---------------------------------------------------------------------------

const reactToolContainer = "fin"

type reactTestHarness struct {
	react  *ir.React
	wf     *ir.Workflow
	runner *scriptedToolLoop
	fake   *container.Fake
	clk    *clock.Fake
	lg     *state.InMemoryLog
	blobs  *state.InMemoryBlobs
	rs     *RunState
	ld     *LocalDispatcher
	input  map[string]any // run input bound into the node scope ({{ input.* }}); nil → no input
}

func newReactTestHarness(t *testing.T, r *ir.React, wf *ir.Workflow, runner *scriptedToolLoop) *reactTestHarness {
	t.Helper()
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: reactToolContainer})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	var reg agent.Registry
	if err := reg.Register(runner); err != nil {
		t.Fatalf("register runner: %v", err)
	}
	clk := &clock.Fake{T: testClockEpoch}
	return &reactTestHarness{
		react:  r,
		wf:     wf,
		runner: runner,
		fake:   fake,
		clk:    clk,
		lg:     state.NewInMemoryLog(clk),
		blobs:  state.NewInMemoryBlobs(),
		rs:     NewRunState(testRunID, testDigest, nil),
		ld: &LocalDispatcher{
			Backend:  fake,
			Handles:  map[string]container.Handle{reactToolContainer: h},
			Resolver: &reg,
		},
	}
}

func (h *reactTestHarness) ictx() interpreterContext {
	return interpreterContext{
		wf:         h.wf,
		runstate:   h.rs,
		dispatcher: h.ld,
		log:        h.lg,
		blobs:      h.blobs,
		clk:        h.clk,
		input:      h.input,
	}
}

func (h *reactTestHarness) run(t *testing.T) (Outcome, error) {
	t.Helper()
	return runReactWithContext(context.Background(), h.react, "react[0]", h.ictx())
}

// programTool programs the fake backend to return result for the impl run: of
// the named tool. The impl run: is matched verbatim (the synthesized command).
func (h *reactTestHarness) programTool(run string, result container.ExecResult) {
	h.fake.ProgramExec(run, result, nil)
}

// programToolWithFiles is programTool plus the files the programmed command
// WRITES into the executing handle's fs when it runs — so the dispatcher's
// post-Exec CaptureFiles (output_files capture, exit==0 only) finds them.
func (h *reactTestHarness) programToolWithFiles(run string, result container.ExecResult, files map[string][]byte) {
	h.fake.ProgramExecWithFiles(run, result, nil, files)
}

// reactWorkflowWithOutputFiles builds a one-tool workflow whose impl run: is
// `toolRun` and whose impl declares a single output_files entry with a TEMPLATED
// path (exercising toolImplScope inside an output_files path, spec §4.5).
func reactWorkflowWithOutputFiles(toolRun, outputFilePath string) *ir.Workflow {
	wf := reactWorkflow(toolRun)
	tool := wf.Tools["check"]
	tool.Impl.OutputFiles = ir.OutputFiles{{Name: "report", Path: outputFilePath}}
	wf.Tools["check"] = tool
	return wf
}

// reactWorkflow builds a one-tool workflow whose impl run: command is `run`.
func reactWorkflow(toolRun string) *ir.Workflow {
	return &ir.Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]ir.Container{
			reactToolContainer: {Image: "oci://example.com/fin@sha256:abc"},
		},
		Tools: map[string]ir.Tool{
			"check": {
				Description: "check something",
				InputSchema: &ir.JSONSchema{"type": "object"},
				Impl:        ir.ToolImpl{Run: toolRun, Container: reactToolContainer},
			},
		},
		Graph: ir.NodeList{},
	}
}

// ---------------------------------------------------------------------------
// Task 4.2 — natural stop, single round.
// ---------------------------------------------------------------------------

func TestRunReactNaturalStopOneRound(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{
		ID:     "answer",
		Prompt: "what is 6*7?",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: `{"answer":"42"}`, FinishReason: "stop", Output: map[string]any{"answer": "42"}},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	nr, ok := h.rs.LookupCompleted("react[0]")
	if !ok {
		t.Fatal("react[0] not committed")
	}
	if nr.Outputs["answer"] != "42" || nr.Outputs["stop_reason"] != "stop" {
		t.Fatalf("terminal output = %v", nr.Outputs)
	}
	// The model leaf for round 1 must also be committed.
	if _, ok := h.rs.LookupCompleted("react[0].round-1.model"); !ok {
		t.Fatal("round-1.model leaf not committed")
	}
	// A single model call (one round, natural stop).
	if got := len(h.runner.Calls()); got != 1 {
		t.Fatalf("model called %d times, want 1", got)
	}
	// The invocation carried the prompt as the initial user turn + the tools + schema.
	inv := h.runner.Calls()[0]
	if len(inv.Messages) != 1 || inv.Messages[0].Role != "user" || inv.Messages[0].Content != "what is 6*7?" {
		t.Fatalf("initial messages = %+v", inv.Messages)
	}
	if len(inv.Tools) != 1 || inv.Tools[0].Name != "check" {
		t.Fatalf("tools = %+v", inv.Tools)
	}
	if inv.OutputSchema == nil {
		t.Fatal("invocation OutputSchema not forwarded")
	}
	if inv.NodePath != "react[0].round-1.model" {
		t.Fatalf("NodePath = %q", inv.NodePath)
	}
}

// hasToolMessage reports whether msgs contains a tool-role message with the
// given tool_call_id whose content contains substr.
func hasToolMessage(msgs []agent.ReactTurn, id, substr string) bool {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

func assertToolMessage(t *testing.T, msgs []agent.ReactTurn, id, substr string) {
	t.Helper()
	if !hasToolMessage(msgs, id, substr) {
		t.Fatalf("messages missing tool result id=%q substr=%q: %+v", id, substr, msgs)
	}
}

// assertAssistantToolCall asserts an assistant turn carrying a tool_call with the
// given id/name and VERBATIM arguments string.
func assertAssistantToolCall(t *testing.T, msgs []agent.ReactTurn, id, name, args string) {
	t.Helper()
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == id && tc.Name == name && tc.Arguments == args {
				return
			}
		}
	}
	t.Fatalf("messages missing assistant tool_call id=%q name=%q args=%q: %+v", id, name, args, msgs)
}

// ---------------------------------------------------------------------------
// Task 4.3 — dispatch one tool, then answer; the result is fed to round 2 with
// a matching tool_call_id, the round-1 marker is committed.
// ---------------------------------------------------------------------------

func TestRunReactDispatchesToolThenAnswers(t *testing.T) {
	// The impl reads the staged args file — substitution produces a deterministic
	// command the fake matches verbatim.
	wf := reactWorkflow("./check {{ args_file }}")
	r := &ir.React{
		ID:     "answer",
		Prompt: "use the tool",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"x":1}`}}, FinishReason: "tool_calls"},
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	// argsFilePath("react[0].round-1.tool-0", "/work/.awf") → /work/.awf/react_0__round-1_tool-0.args.json
	wantArgs := argsFilePath("react[0].round-1.tool-0", "/work/.awf")
	h.programTool("./check "+wantArgs, container.ExecResult{ExitCode: 0, Stdout: []byte("RESULT-OK")})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	if _, ok := h.rs.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Fatal("tool-0 leaf not committed")
	}
	// Round 1 dispatched tools → its react.round marker committed. Round 2
	// natural-stops (terminal R is the anchor — no marker).
	if got := len(h.rs.LookupReactRounds("react[0]")); got != 1 {
		t.Fatalf("rounds recorded = %d, want 1", got)
	}
	// Two model calls (round 1 requested a tool, round 2 answered).
	if got := len(h.runner.Calls()); got != 2 {
		t.Fatalf("model called %d times, want 2", got)
	}
	// Round 2's invocation must include the tool result with matching id.
	call2 := h.runner.Calls()[1]
	if !hasToolMessage(call2.Messages, "c1", "RESULT-OK") {
		t.Fatalf("round-2 messages missing tool result: %+v", call2.Messages)
	}
	// Round 2's history must also carry the round-1 assistant(tool_calls).
	assertAssistantToolCall(t, call2.Messages, "c1", "check", `{"x":1}`)
	// The verbatim args were staged to the per-call file.
	tnr, _ := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if tnr.Outputs["stdout"] != "RESULT-OK" {
		t.Fatalf("tool leaf stdout = %v", tnr.Outputs["stdout"])
	}
}

// Task 4.3 — a tool's non-zero exit feeds exit+stdout back; the react step does
// NOT fail; the leaf commits OK.
func TestRunReactToolNonZeroExitFedBack(t *testing.T) {
	wf := reactWorkflow("./check {{ args_file }}")
	r := &ir.React{ID: "a", Prompt: "go", Tools: []string{"check"}, With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: "{}"}}, FinishReason: "tool_calls"},
		{Text: "recovered", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	wantArgs := argsFilePath("react[0].round-1.tool-0", "/work/.awf")
	h.programTool("./check "+wantArgs, container.ExecResult{ExitCode: 3, Stdout: []byte("boom")})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v (a tool's non-zero exit must not fail the react step)", oc, err)
	}
	tnr, ok := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if !ok {
		t.Fatal("tool leaf not committed on non-zero exit")
	}
	if jsonInt(tnr.Outputs["exit_code"]) != 3 {
		t.Fatalf("tool leaf exit_code = %v, want 3", tnr.Outputs["exit_code"])
	}
	// The model-facing message carries the exit prefix + stdout.
	call2 := h.runner.Calls()[1]
	assertToolMessage(t, call2.Messages, "c1", "[exit 3]")
	assertToolMessage(t, call2.Messages, "c1", "boom")
}

// Task 4.3 — an unknown/hallucinated tool name feeds an error tool message; no
// dispatch; the loop proceeds.
func TestRunReactUnknownToolFedBack(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{ID: "a", Prompt: "go", Tools: []string{"check"}, With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "ghost", Arguments: "{}"}}, FinishReason: "tool_calls"},
		{Text: "ok", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	if _, ok := h.rs.LookupCompleted("react[0].round-1.tool-0"); ok {
		t.Fatal("unknown tool must NOT commit a tool leaf")
	}
	call2 := h.runner.Calls()[1]
	assertToolMessage(t, call2.Messages, "c1", "unknown tool")
}

// ---------------------------------------------------------------------------
// Task 4.4 — replay: a committed round-1 (model leaf w/ tool_call + tool-0 leaf
// + marker) is rebuilt from the log on resume; round 1 is NOT re-sampled.
// ---------------------------------------------------------------------------

// seedCommittedRound1 commits a round-1 model leaf (one tool_call) + its tool-0
// leaf + the react.round marker, then RE-FOLDS the log into h.rs so the resume
// RunState is exactly what a real resume would rebuild (ints become float64,
// arguments round-trip as a verbatim string). After this, runReact must resume
// at round 2.
func (h *reactTestHarness) seedCommittedRound1(t *testing.T, toolCallID, toolName, args, toolStdout string) {
	t.Helper()
	ictx := h.ictx()
	// 0. A run.started event must lead the log (Fold requires it).
	rsData, err := json.Marshal(RunStartedData{RunID: testRunID, WorkflowDigest: testDigest})
	if err != nil {
		t.Fatalf("marshal run.started: %v", err)
	}
	if err := h.lg.Append(state.Event{Type: EventRunStarted, Data: rsData}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	// 1. Commit the model leaf with a single tool_call.
	mr := modelResult{
		Text:         "",
		FinishReason: "tool_calls",
		ToolCalls:    []agent.ToolCall{{Index: 0, ID: toolCallID, Name: toolName, Arguments: args}},
	}
	if err := commitModelLeaf(ictx, "react[0].round-1.model", mr); err != nil {
		t.Fatalf("seed model leaf: %v", err)
	}
	// 2. Commit the tool-0 leaf carrying the (already-bounded) model-facing content.
	toolDR := DispatchResult{
		Outcome:  OutcomeOK,
		ExitCode: copyIntPtr(0),
		Outputs:  map[string]any{"exit_code": 0, "stdout": toolStdout, "content": toolStdout},
		Stdout:   []byte(toolStdout),
	}
	tnr, err := Commit(ictx.log, ictx.blobs, "react[0].round-1.tool-0", toolDR, false)
	if err != nil {
		t.Fatalf("seed tool leaf: %v", err)
	}
	ictx.runstate.RecordCompleted("react[0].round-1.tool-0", tnr)
	// 3. Close round 1 (marker).
	if err := closeRound(ictx, "react[0]", 1); err != nil {
		t.Fatalf("seed close round: %v", err)
	}
	// 4. Re-fold the log into a fresh RunState — the true resume path.
	events, err := h.lg.Fold()
	if err != nil {
		t.Fatalf("fold seeded log: %v", err)
	}
	folded, err := Fold(events, h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	h.rs = folded
}

func TestRunReactReplaysCommittedRound(t *testing.T) {
	wf := reactWorkflow("./check {{ args_file }}")
	r := &ir.React{ID: "answer", Prompt: "use the tool", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	// Only ONE script entry — it must be consumed by ROUND 2 (round 1 replayed).
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "final", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	h.seedCommittedRound1(t, "c1", "check", `{"x":1}`, "RES")

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	// Round 1 was replayed, not re-sampled: exactly ONE model call (round 2).
	if got := len(h.runner.Calls()); got != 1 {
		t.Fatalf("model called %d times, want 1 (round 1 must be replayed, not re-sampled)", got)
	}
	// Round 1 must NOT have re-dispatched its tool (no second exec) — the leaf is
	// the same one we seeded; assert via the single model call's rebuilt history.
	msgs := h.runner.Calls()[0].Messages
	// initial user turn + round-1 assistant(tool_calls) + round-1 tool(result)
	if len(msgs) != 3 {
		t.Fatalf("replayed history len = %d, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "use the tool" {
		t.Fatalf("initial user turn = %+v", msgs[0])
	}
	assertAssistantToolCall(t, msgs, "c1", "check", `{"x":1}`)
	assertToolMessage(t, msgs, "c1", "RES")
	// Terminal committed.
	nr, ok := h.rs.LookupCompleted("react[0]")
	if !ok || nr.Outputs["text"] != "final" || nr.Outputs["stop_reason"] != "stop" {
		t.Fatalf("terminal = %v (ok=%v)", nr.Outputs, ok)
	}
}

// ---------------------------------------------------------------------------
// Task 4.5 — max_turns truncation: the model keeps requesting tools but the
// budget is exhausted; the dangling round's tools are NOT dispatched; the
// terminal is {stop_reason:"max_turns", text}; output_schema NOT enforced.
// ---------------------------------------------------------------------------

func TestRunReactMaxTurnsTruncates(t *testing.T) {
	wf := reactWorkflow("./check {{ args_file }}")
	r := &ir.React{
		ID:       "answer",
		Prompt:   "go",
		Tools:    []string{"check"},
		MaxTurns: 1,
		With:     ir.RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "partial", ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: "{}"}}, FinishReason: "tool_calls"},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	if _, ok := h.rs.LookupCompleted("react[0].round-1.tool-0"); ok {
		t.Fatal("max_turns must NOT dispatch the dangling round's tools")
	}
	nr, ok := h.rs.LookupCompleted("react[0]")
	if !ok {
		t.Fatal("terminal not committed")
	}
	if nr.Outputs["stop_reason"] != "max_turns" || nr.Outputs["text"] != "partial" {
		t.Fatalf("terminal = %v", nr.Outputs)
	}
	if _, hasAnswer := nr.Outputs["answer"]; hasAnswer {
		t.Fatal("output_schema must NOT be enforced on max_turns truncation")
	}
	// No react.round marker for a truncated frontier round (terminal R is the anchor).
	if got := len(h.rs.LookupReactRounds("react[0]")); got != 0 {
		t.Fatalf("rounds recorded = %d, want 0 (truncated round commits no marker)", got)
	}
}

// Task 4.5 — natural stop with output_schema: the engine validates the
// adapter-parsed Output (ValidateOutputMap); a parse miss surfaced by the
// adapter as *ErrUnparseableOutput is OutcomeRetryableFailure (not a crash).
func TestRunReactNaturalStopSchemaMissIsRetryable(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{
		ID:     "a",
		Prompt: "q",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
	}
	runner := &scriptedToolLoop{
		results: []agent.ToolLoopResult{{Text: "not json", FinishReason: "stop"}},
		errs:    []error{&agent.ErrUnparseableOutput{NodePath: "react[0].round-1.model"}},
	}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if oc != OutcomeRetryableFailure || err == nil {
		t.Fatalf("run: oc=%q err=%v, want retryable_failure + error", oc, err)
	}
	// No terminal committed on a parse miss.
	if _, ok := h.rs.LookupCompleted("react[0]"); ok {
		t.Fatal("terminal must NOT commit on an unparseable-output miss")
	}
}

// Task 4.5 — natural stop with output_schema: the engine REJECTS a parsed
// Output that violates the schema (ValidateOutputMap), as retryable_failure.
func TestRunReactNaturalStopSchemaViolationIsRetryable(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{
		ID:     "a",
		Prompt: "q",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
	}
	// Parsed object is missing the required "answer" field.
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: `{"other":1}`, FinishReason: "stop", Output: map[string]any{"other": float64(1)}},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if oc != OutcomeRetryableFailure || err == nil {
		t.Fatalf("run: oc=%q err=%v, want retryable_failure + error", oc, err)
	}
}

// ---------------------------------------------------------------------------
// Task 4.6 — boundToolResult: byte-cap the model-facing tool output; non-UTF-8
// becomes a descriptor (never raw/corrupted bytes).
// ---------------------------------------------------------------------------

func TestBoundToolResult(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 20000)
	out := boundToolResult(container.ExecResult{ExitCode: 0, Stdout: big})
	if len(out) > 17000 { // 16384 + marker headroom
		t.Fatalf("not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("missing truncation marker: %q", out[len(out)-64:])
	}

	// non-UTF-8 → descriptor, not raw bytes.
	bin := boundToolResult(container.ExecResult{ExitCode: 0, Stdout: []byte{0xff, 0xfe, 0x00}})
	if strings.ContainsRune(bin, '�') || strings.Contains(bin, "\x00") {
		t.Fatalf("binary output must be a descriptor, not inlined bytes: %q", bin)
	}
	if !strings.Contains(bin, "non-text tool output") {
		t.Fatalf("missing non-text descriptor: %q", bin)
	}

	// non-zero exit on valid UTF-8 gets an [exit N] prefix.
	nz := boundToolResult(container.ExecResult{ExitCode: 7, Stdout: []byte("oops")})
	if !strings.HasPrefix(nz, "[exit 7]") {
		t.Fatalf("missing exit prefix: %q", nz)
	}
	if !strings.Contains(nz, "oops") {
		t.Fatalf("lost stdout: %q", nz)
	}

	// small valid UTF-8, exit 0 → verbatim (no marker, no prefix).
	ok := boundToolResult(container.ExecResult{ExitCode: 0, Stdout: []byte("hello")})
	if ok != "hello" {
		t.Fatalf("small ok output mangled: %q", ok)
	}
}

// ---------------------------------------------------------------------------
// Frontier resume (spec §4.2/§4.4): the .model leaf of the frontier round is
// committed but its tool leaf is NOT, and no react.round marker was written
// (torn frontier). On resume the loop re-enters that round, reads the model leaf
// back (NO re-sample), re-dispatches only the uncommitted tool, then closes the
// round and continues.
// ---------------------------------------------------------------------------

func TestRunReactResumesTornFrontierModelCommittedToolNot(t *testing.T) {
	wf := reactWorkflow("./check {{ args_file }}")
	r := &ir.React{ID: "answer", Prompt: "go", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	// Round 1's model leaf is pre-committed (tool_calls). The runner must NOT be
	// called for round 1 — only round 2's answer is scripted.
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	// Seed: run.started + round-1 model leaf only (NO tool leaf, NO marker), then
	// re-fold so the resume RunState has the committed model leaf but no round.
	rsData, err := json.Marshal(RunStartedData{RunID: testRunID, WorkflowDigest: testDigest})
	if err != nil {
		t.Fatalf("marshal run.started: %v", err)
	}
	if err := h.lg.Append(state.Event{Type: EventRunStarted, Data: rsData}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	mr := modelResult{FinishReason: "tool_calls",
		ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"x":1}`}}}
	if err := commitModelLeaf(h.ictx(), "react[0].round-1.model", mr); err != nil {
		t.Fatalf("seed model leaf: %v", err)
	}
	events, err := h.lg.Fold()
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	folded, err := Fold(events, h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	h.rs = folded
	// The frontier round has no marker → startK is still 1.
	if got := len(h.rs.LookupReactRounds("react[0]")); got != 0 {
		t.Fatalf("seeded rounds = %d, want 0 (torn frontier, no marker)", got)
	}

	// Program the (only) uncommitted tool dispatch.
	wantArgs := argsFilePath("react[0].round-1.tool-0", "/work/.awf")
	h.programTool("./check "+wantArgs, container.ExecResult{ExitCode: 0, Stdout: []byte("FRESH")})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	// Round 1's model was NOT re-sampled — the only model call is round 2.
	if got := len(h.runner.Calls()); got != 1 {
		t.Fatalf("model called %d times, want 1 (frontier model leaf must replay)", got)
	}
	// The uncommitted tool got dispatched fresh and committed.
	tnr, ok := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if !ok || tnr.Outputs["stdout"] != "FRESH" {
		t.Fatalf("tool leaf = %v (ok=%v)", tnr.Outputs, ok)
	}
	// Round 1 now closed; terminal committed.
	if got := len(h.rs.LookupReactRounds("react[0]")); got != 1 {
		t.Fatalf("rounds after resume = %d, want 1", got)
	}
	// Round 2's history carries the fresh tool result with the matching id.
	assertToolMessage(t, h.runner.Calls()[0].Messages, "c1", "FRESH")
}

// ---------------------------------------------------------------------------
// Task 4.5 — output_files capture: a tool impl declaring a TEMPLATED
// output_files path resolves it via toolImplScope, the produced file lands on
// the .tool-J leaf's Files map, and it is NOT surfaced to the model (the
// "captured-but-not-surfaced" contract — the model sees only stdout). Plus the
// missing-file sub-case (declared but not produced → exit-code present →
// rewrite-to-OK-leaf branch, capture silently empty).
// ---------------------------------------------------------------------------

func TestRunReactCapturesTemplatedOutputFile(t *testing.T) {
	// The impl run: is a fixed key (so the fake lookup is stable); the TEMPLATED
	// bit lives in the output_files path: "/work/out-{{ args.x }}.json".
	wf := reactWorkflowWithOutputFiles("produce", "/work/out-{{ args.x }}.json")
	r := &ir.React{ID: "answer", Prompt: "go", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"x":"7"}`}}, FinishReason: "tool_calls"},
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	// args.x == "7" → the templated capture path resolves to /work/out-7.json.
	// The fake writes that exact path when "produce" runs; the dispatcher captures
	// it (exit 0, output_files non-empty). STDOUT is the only model-facing surface.
	const capturedPath = "/work/out-7.json"
	const capturedBytes = "SECRET-FILE-PAYLOAD"
	h.programToolWithFiles("produce",
		container.ExecResult{ExitCode: 0, Stdout: []byte("STDOUT-VISIBLE")},
		map[string][]byte{capturedPath: []byte(capturedBytes)})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}

	// (a)+(b) The TEMPLATED path resolved (via toolImplScope) and the captured
	// file landed on the .tool-0 leaf's Files map under the RESOLVED path.
	tnr, ok := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if !ok {
		t.Fatal("tool-0 leaf not committed")
	}
	ref, ok := tnr.Files[capturedPath]
	if !ok {
		t.Fatalf("captured file %q not on the tool leaf Files map: %v", capturedPath, tnr.Files)
	}
	got, err := h.blobs.Get(ref)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", ref, err)
	}
	if string(got) != capturedBytes {
		t.Fatalf("captured file content = %q, want %q", got, capturedBytes)
	}

	// (c) The captured file is NOT surfaced to the model: round 2's tool message
	// for c1 carries stdout, never the file's bytes (§4.5 captured-but-not-surfaced).
	call2 := h.runner.Calls()[1]
	assertToolMessage(t, call2.Messages, "c1", "STDOUT-VISIBLE")
	for _, m := range call2.Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" && strings.Contains(m.Content, capturedBytes) {
			t.Fatalf("captured file bytes leaked into the model-facing tool message: %q", m.Content)
		}
	}
}

// Missing-file sub-case: a tool exits 0 but fails to produce its declared
// output_file → the dispatcher's CaptureFiles errors → DispatchResult is
// retryable_failure WITH an ExitCode set. dispatchOneTool takes the
// `dr.ExitCode != nil` "rewrite to OK leaf + feed stdout back" branch (capture
// silently empty), so the react step does NOT fail and the model sees stdout.
func TestRunReactMissingOutputFileRewritesToOKLeaf(t *testing.T) {
	wf := reactWorkflowWithOutputFiles("produce", "/work/out-{{ args.x }}.json")
	r := &ir.React{ID: "answer", Prompt: "go", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"x":"7"}`}}, FinishReason: "tool_calls"},
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	// Exit 0 but NO files written → the declared /work/out-7.json is absent →
	// CaptureFiles errors → retryable_failure + ExitCode set.
	h.programTool("produce", container.ExecResult{ExitCode: 0, Stdout: []byte("STDOUT-ONLY")})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v (a missing declared output_file must not fail the react step)", oc, err)
	}
	// The leaf committed OK (rewrite branch), exit_code 0, capture silently empty.
	tnr, ok := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if !ok {
		t.Fatal("tool-0 leaf not committed on the rewrite branch")
	}
	if jsonInt(tnr.Outputs["exit_code"]) != 0 {
		t.Fatalf("tool leaf exit_code = %v, want 0", tnr.Outputs["exit_code"])
	}
	if len(tnr.Files) != 0 {
		t.Fatalf("capture must be silently empty on the rewrite branch, got Files=%v", tnr.Files)
	}
	// The model still gets stdout fed back.
	assertToolMessage(t, h.runner.Calls()[1].Messages, "c1", "STDOUT-ONLY")
}

// ---------------------------------------------------------------------------
// P3 review fix — tool-impl input_files staging. A react tool whose impl declares
// input_files: stages the referenced asset bytes into the tool container BEFORE
// exec, mirroring the already-wired output_files path. Verified end-to-end: stage
// asset bytes to a path AND declare an output_files capture at that SAME path; the
// programmed exec writes nothing there, so the only bytes CaptureFiles can read
// back are the ones CopyTo (input_files staging) wrote → proving the InputFile
// reached the container.
// ---------------------------------------------------------------------------

func TestRunReactStagesToolImplInputFileAsset(t *testing.T) {
	const stagedPath = "/work/fixture.json"
	const assetBytes = "STAGED-ASSET-PAYLOAD"

	wf := reactWorkflow("consume")
	// Declare the asset on the workflow + record it in the run-start manifest so
	// resolveInputFiles can fetch its bytes.
	wf.Assets = map[string]string{"fixture": "fixtures/fixture.json"}
	tool := wf.Tools["check"]
	tool.Impl.InputFiles = map[string]string{stagedPath: "asset.fixture"}
	// Capture the SAME path the input file was staged to; the exec writes nothing
	// there, so what we read back must be the staged input bytes.
	tool.Impl.OutputFiles = ir.OutputFiles{{Name: "echo", Path: stagedPath}}
	wf.Tools["check"] = tool

	r := &ir.React{ID: "answer", Prompt: "go", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{}`}}, FinishReason: "tool_calls"},
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	// Seed the asset content into Blobs + record the run-start manifest entry.
	ref, err := h.blobs.Put([]byte(assetBytes))
	if err != nil {
		t.Fatalf("seed asset blob: %v", err)
	}
	h.rs.Assets = map[string]RunStartedAsset{
		"fixture": testRunStartedAsset("fixtures/fixture.json", ref, []byte(assetBytes)),
	}

	// The programmed exec produces NO files — the only bytes at stagedPath come
	// from input_files staging (CopyTo).
	h.programTool("consume", container.ExecResult{ExitCode: 0, Stdout: []byte("ran")})

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}

	tnr, ok := h.rs.LookupCompleted("react[0].round-1.tool-0")
	if !ok {
		t.Fatal("tool-0 leaf not committed")
	}
	cas, ok := tnr.Files[stagedPath]
	if !ok {
		t.Fatalf("captured file %q not on the tool leaf Files map: %v", stagedPath, tnr.Files)
	}
	got, err := h.blobs.Get(cas)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", cas, err)
	}
	if string(got) != assetBytes {
		t.Fatalf("staged-then-captured content = %q, want %q (the InputFile must have reached CopyTo)", got, assetBytes)
	}
}

// A tool impl whose input_files dst would collide with the per-call verbatim args
// file path is a hard react-step failure (the merged-collision guard). The args
// file lives under /work/.awf/...; staging an input there is rejected.
func TestRunReactToolImplInputFileCollidesWithArgsFile(t *testing.T) {
	wf := reactWorkflow("consume")
	wf.Assets = map[string]string{"fixture": "fixtures/fixture.json"}
	tool := wf.Tools["check"]
	// Stage the asset to the EXACT args-file path for this tool call → collision.
	collidePath := argsFilePath("react[0].round-1.tool-0", "/work/.awf")
	tool.Impl.InputFiles = map[string]string{collidePath: "asset.fixture"}
	wf.Tools["check"] = tool

	r := &ir.React{ID: "answer", Prompt: "go", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{}`}}, FinishReason: "tool_calls"},
		{Text: "done", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	ref, err := h.blobs.Put([]byte("x"))
	if err != nil {
		t.Fatalf("seed asset blob: %v", err)
	}
	h.rs.Assets = map[string]RunStartedAsset{
		"fixture": testRunStartedAsset("fixtures/fixture.json", ref, []byte("x")),
	}
	h.programTool("consume", container.ExecResult{ExitCode: 0, Stdout: []byte("ran")})

	_, err = h.run(t)
	if err == nil {
		t.Fatal("expected a hard react-step failure on args-file path collision, got nil")
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Fatalf("error = %v, want a path-collision error", err)
	}
}

// ---------------------------------------------------------------------------
// Slice 6.1 parity — the adapter's per-round Metrics ride onto the .model leaf's
// node.completed (verbatim; observational, not folded into resume).
// ---------------------------------------------------------------------------

func TestRunReactModelLeafCarriesMetrics(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{ID: "answer", Prompt: "q", Tools: []string{"check"},
		With: ir.RawConfig{"uses": "awf/llm", "model": "m"}}
	ms := &agent.MetricSet{
		Cost:   agent.MetricCost{Total: 0.42, Source: agent.CostSourceReported},
		Tokens: agent.MetricTokens{Input: 100, Output: 50},
		Turns:  1,
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "hi", FinishReason: "stop", Metrics: ms},
	}}
	h := newReactTestHarness(t, r, wf, runner)

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}
	// The .model leaf's node.completed event must carry the Metrics verbatim.
	events, err := h.lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var got *agent.MetricSet
	for _, e := range events {
		if e.Type != EventNodeCompleted || e.Path != "react[0].round-1.model" {
			continue
		}
		var d NodeCompletedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal node.completed: %v", err)
		}
		got = d.Metrics
	}
	if got == nil {
		t.Fatal("the .model leaf's node.completed carries no Metrics (react cost is invisible to obs)")
	}
	if got.Cost.Total != 0.42 || got.Tokens.Input != 100 || got.Tokens.Output != 50 {
		t.Fatalf("model-leaf Metrics = %+v, want cost 0.42 tokens 100/50", got)
	}
}

// ---------------------------------------------------------------------------
// react.prompt templating (spec §3.2 "prompt — the initial user message,
// templated, scalars only"). The initial user turn must be substituted against
// the react node's engine scope, not passed verbatim — mirroring how a code
// step / agent step templates. Determinism: the prompt binds input.* (fixed at
// run-start) + prior committed step.*, so the same substituted string is rebuilt
// on every entry (fresh + resume).
// ---------------------------------------------------------------------------

func TestRunReactTemplatesPrompt(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{
		ID:     "answer",
		Prompt: "answer {{ input.q }}",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "ok", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	h.input = map[string]any{"q": "X"}

	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}

	calls := h.runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(calls))
	}
	first := calls[0].Messages
	if len(first) != 1 || first[0].Role != "user" {
		t.Fatalf("initial messages = %+v, want one user turn", first)
	}
	if first[0].Content != "answer X" {
		t.Fatalf("model received prompt %q, want %q (react.prompt must be templated)", first[0].Content, "answer X")
	}
}

// TestRunReactPromptTemplateErrorIsPermanentFailure: a bad prompt template ref
// (unresolved) is a permanent failure (the model can't fix the config), exactly
// like a code step's bad run: template — NOT an internal halt, NOT a retry.
func TestRunReactPromptTemplateErrorIsPermanentFailure(t *testing.T) {
	wf := reactWorkflow("true")
	r := &ir.React{
		ID:     "answer",
		Prompt: "answer {{ input.nope }}",
		Tools:  []string{"check"},
		With:   ir.RawConfig{"uses": "awf/llm", "model": "m"},
	}
	runner := &scriptedToolLoop{results: []agent.ToolLoopResult{
		{Text: "ok", FinishReason: "stop"},
	}}
	h := newReactTestHarness(t, r, wf, runner)
	h.input = map[string]any{"q": "X"} // no "nope" key → AWF4002

	oc, err := h.run(t)
	if oc != OutcomePermanentFailure || err == nil {
		t.Fatalf("run: oc=%q err=%v, want permanent_failure + error (bad prompt template ref)", oc, err)
	}
	// The model was never called — the prompt failed before the first round.
	if got := len(h.runner.Calls()); got != 0 {
		t.Fatalf("model called %d times, want 0 (template error precedes any model call)", got)
	}
	// No terminal committed on a prompt template error.
	if _, ok := h.rs.LookupCompleted("react[0]"); ok {
		t.Fatal("terminal must NOT commit on a prompt template error")
	}
}
