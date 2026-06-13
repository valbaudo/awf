package engine

import (
	"context"
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
	// argsFilePath("react[0].round-1.tool-0") → /work/.awf/react_0__round-1_tool-0.args.json
	wantArgs := argsFilePath("react[0].round-1.tool-0")
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
	wantArgs := argsFilePath("react[0].round-1.tool-0")
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
