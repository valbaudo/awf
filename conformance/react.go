package conformance

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// reactToolLoopWorkflow is the happy-path react: fixture (Task 5.2): a top-level
// tools: block with one tool whose impl run: reads {{ args_file }}, a top-level
// react: step on the awf/llm path, and an outputs: map referencing the react
// node's typed answer (proving react producer registration end-to-end). The
// container is fakeImageDigest-pinned like every other fixture. max_turns: 4
// leaves head-room above the scripted two-round loop (round 1 = one tool call;
// round 2 = natural stop with the typed answer).
//
// react.prompt is a {{ input.q }} TEMPLATE (spec §3.2 "prompt — the initial user
// message, templated, scalars only"): the engine substitutes it against the
// react node scope, so the model's initial user turn carries the resolved value
// (reactPromptInput["q"]). Every sub-test binds reactPromptInput via h.input so
// the prompt resolves on the run (and identically on resume — the determinism
// invariant).
var reactToolLoopWorkflow = fmt.Sprintf(`workflow: conformance-react-tool-loop
version: 1
input:
  type: object
  additionalProperties: false
  required: [q]
  properties:
    q: { type: string }
output_schema:
  type: object
  additionalProperties: false
  required: [final]
  properties:
    final: { type: string }
containers:
  fin:
    image: %s
tools:
  check:
    description: echo the staged args
    input_schema:
      type: object
      additionalProperties: false
      required: [iban]
      properties:
        iban: { type: string }
    impl:
      run: "cat {{ args_file }}"
      container: fin
graph:
  - react:
      id: answer
      with: { uses: awf/llm, model: m }
      prompt: "validate {{ input.q }}"
      tools: [check]
      max_turns: 4
      output_schema:
        type: object
        additionalProperties: false
        required: [answer]
        properties:
          answer: { type: string }
outputs:
  final: "{{ answer.answer }}"
`, fakeImageDigest)

// reactPromptInput is the run input bound by every react sub-test. The react
// node's prompt `"validate {{ input.q }}"` resolves to reactPromptExpanded.
var reactPromptInput = map[string]any{"q": "the staged iban"}

// reactPromptExpanded is the substituted initial user turn the model must
// receive — proof that react.prompt is templated (not passed verbatim).
const reactPromptExpanded = "validate the staged iban"

// reactToolEchoResult is the bytes the fake `cat {{ args_file }}` tool returns —
// the verbatim staged arguments. ProgramExecAny matches the engine-synthesized
// command regardless of the per-call args-file path (which embeds the runtime
// tool path and is derived inside the engine, not visible to the bucket).
var reactToolEchoResult = container.ExecResult{ExitCode: 0, Stdout: []byte(`{"iban":"DE89"}`)}

// programReactTool returns a factory that programs the fake container backend to
// answer ANY tool-impl exec with reactToolEchoResult (the engine derives the
// per-call args-file path internally, so an exact-match ProgramExec key is not
// addressable from the bucket).
func programReactTool(factory BackendFactory) BackendFactory {
	return func() container.Backend {
		b := factory()
		f, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		f.ProgramExecAny(reactToolEchoResult, nil)
		return f
	}
}

// registerReactLLM builds the register callback for a fake awf/llm ToolLoopRunner
// scripted with the supplied results (indexed by RunToolLoop call). The minted
// *fake.Fake is returned via the out pointer so the bucket can inspect
// ToolLoopCalls() afterwards. mutate is an optional hook applied to the fake
// before registration (e.g. WithToolLoopTripwire on the resume lifetime).
func registerReactLLM(t *testing.T, out **fake.Fake, mutate func(*fake.Fake) *fake.Fake, scripts map[int]agent.ToolLoopResult) func(*agent.Registry) {
	t.Helper()
	return func(reg *agent.Registry) {
		f := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
		if mutate != nil {
			f = mutate(f)
		}
		for i, r := range scripts {
			f = f.ScriptToolLoop(i, r)
		}
		if err := reg.Register(f); err != nil {
			t.Fatalf("register awf/llm: %v", err)
		}
		*out = f
	}
}

// testReact is the react: conformance bucket (Tasks 5.2–5.4): the model+tools
// loop driven end-to-end through engine.Run + the load→validate→run→resume
// harness, against the fake container backend (tool impls) + a fake
// agent.ToolLoopRunner (the model). Sub-tests cover the happy path, resume
// (rounds replay / model not re-sampled / matching tool_call_ids / torn
// frontier), a two-same-tool round, and the agents:-role forwarding path.
func testReact(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("happy_path", func(t *testing.T) { testReactHappyPath(t, factory) })
	t.Run("resume_rounds_replay", func(t *testing.T) { testReactResumeRoundsReplay(t, factory) })
	t.Run("resume_torn_frontier", func(t *testing.T) { testReactResumeTornFrontier(t, factory) })
	t.Run("two_same_tool_in_round", func(t *testing.T) { testReactTwoSameToolInRound(t, factory) })
	t.Run("via_agents_role", func(t *testing.T) { testReactViaAgentsRole(t, factory) })
}

// testReactHappyPath (Task 5.2): a two-round loop (tool call → natural stop)
// runs to completion. Asserts OutcomeOK, the terminal react[0] output (the typed
// answer + the reserved stop_reason sibling), the workflow-level output binding,
// and that the round-1 tool leaf committed at react[0].round-1.tool-0.
func testReactHappyPath(t *testing.T, factory BackendFactory) {
	t.Helper()
	var llm *fake.Fake
	register := registerReactLLM(t, &llm, nil, map[int]agent.ToolLoopResult{
		0: {FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{
			{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE89"}`},
		}},
		1: {FinishReason: "stop", Output: map[string]any{"answer": "validated"}, Text: `{"answer":"validated"}`},
	})
	h := newHarnessWithAgentRegistry(t, programReactTool(factory), reactToolLoopWorkflow, register)
	h.input = reactPromptInput

	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold: %v", ferr)
	}

	// Terminal react[0] output: the typed answer plus the reserved stop_reason.
	nr, ok := rs.LookupCompleted("react[0]")
	if !ok {
		t.Fatalf("terminal react[0] not committed")
	}
	if nr.Outputs["answer"] != "validated" {
		t.Errorf("react[0].answer = %v, want %q", nr.Outputs["answer"], "validated")
	}
	if nr.Outputs["stop_reason"] != "stop" {
		t.Errorf("react[0].stop_reason = %v, want %q", nr.Outputs["stop_reason"], "stop")
	}

	// The round-1 tool leaf committed at react[0].round-1.tool-0.
	if _, ok := rs.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Errorf("react[0].round-1.tool-0 leaf missing")
	}
	// The round-1 model leaf committed too (the synthetic .model leaf).
	if _, ok := rs.LookupCompleted("react[0].round-1.model"); !ok {
		t.Errorf("react[0].round-1.model leaf missing")
	}

	// Exactly two model calls (round 1 tool_calls, round 2 natural stop).
	if got := len(llm.ToolLoopCalls()); got != 2 {
		t.Errorf("model called %d times, want 2 (one tool round + one natural-stop round)", got)
	}
	// The initial user turn carries the TEMPLATED prompt: react.prompt
	// "validate {{ input.q }}" substituted against the run scope → the model
	// receives reactPromptExpanded (proof the prompt is templated, not verbatim).
	calls := llm.ToolLoopCalls()
	if len(calls) > 0 {
		first := calls[0].Messages
		if len(first) != 1 || first[0].Role != "user" || first[0].Content != reactPromptExpanded {
			t.Errorf("initial messages = %+v, want one user turn %q (templated prompt)", first, reactPromptExpanded)
		}
	}
}

// testReactResumeRoundsReplay (Task 5.3a): the load-bearing resume proof. The
// first lifetime crashes immediately AFTER round 1 has fully committed (model
// leaf + tool leaf + react.round marker). The second lifetime resumes against a
// DISTINCT fake whose round-1 model call is a TRIPWIRE — if the engine
// re-samples the committed round 1, RunToolLoop hard-errors. Asserts:
//
//   - resume reaches OutcomeOK;
//   - the resume-lifetime fake's model is called exactly once (round 2 only) —
//     round 1 was replayed from the journal, not re-sampled;
//   - the round-2 invocation's Messages replay round-1's assistant(tool_calls) +
//     tool turn with matching tool_call_ids (assistant.tool_calls[0].id ==
//     tool.tool_call_id == "c1").
//
// Crash placement (FailAppendAfterN counts every Append; appendNodeStarted is a
// best-effort Append that still increments the counter):
//
//	k=0 run.started
//	k=1 node.started react[0]            (appendNodeStarted, best-effort)
//	k=2 node.completed round-1.model
//	k=3 node.completed round-1.tool-0
//	k=4 react.round (marker, round 1)
//	k=5 node.completed round-2.model     ← FailAppendAfterN(5) crashes HERE
//
// so round 1 is fully durable (incl. its marker) and round 2 never commits.
func testReactResumeRoundsReplay(t *testing.T, factory BackendFactory) {
	t.Helper()

	round1 := agent.ToolLoopResult{FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{
		{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE89"}`},
	}}
	round2 := agent.ToolLoopResult{FinishReason: "stop", Output: map[string]any{"answer": "validated"}, Text: `{"answer":"validated"}`}

	// First lifetime: both rounds scripted; we crash before round 2 commits.
	var llm1 *fake.Fake
	register1 := registerReactLLM(t, &llm1, nil, map[int]agent.ToolLoopResult{0: round1, 1: round2})
	h := newHarnessWithAgentRegistry(t, programReactTool(factory), reactToolLoopWorkflow, register1)
	h.input = reactPromptInput // first run binds input; resume restores it from run.started's InputRef

	h.log.FailAppendAfterN(5) // crash at round-2's model commit (see ledger above)
	oc1, err1 := h.runWorkflow(t)
	if err1 == nil {
		t.Fatalf("first run: err = nil, want induced-fault crash after round 1")
	}
	if oc1 == engine.OutcomeOK {
		t.Fatalf("first run: outcome = ok, want a crash before the terminal commit")
	}

	// Pre-resume: exactly round 1 committed (model + tool + marker), no terminal.
	preRS, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold pre-resume: %v", ferr)
	}
	if got := len(preRS.LookupReactRounds("react[0]")); got != 1 {
		t.Fatalf("pre-resume rounds = %d, want 1 (round 1 marker durable)", got)
	}
	if _, ok := preRS.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Fatalf("pre-resume: round-1.tool-0 leaf missing")
	}
	if _, ok := preRS.LookupCompleted("react[0]"); ok {
		t.Fatalf("pre-resume: terminal react[0] committed, want absent (crashed before round 2)")
	}

	// Second lifetime: a DISTINCT tripwire fake. WithToolLoopTripwire(1) advances
	// the call index past the 1 committed round and hard-errors if RunToolLoop is
	// invoked for index < 1. Only round 2 (index 1) is scripted.
	h.log.ClearFault()
	var llm2 *fake.Fake
	register2 := registerReactLLM(t, &llm2,
		func(f *fake.Fake) *fake.Fake { return f.WithToolLoopTripwire(1) },
		map[int]agent.ToolLoopResult{1: round2})
	// Re-register on the harness's existing (persisted) registry so the resume
	// lifetime sees the tripwire fake, not the crashed first-lifetime fake.
	h.agentRegistry = &agent.Registry{}
	register2(h.agentRegistry)

	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("resume: err = %v, want nil", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resume: outcome = %q, want ok", oc2)
	}

	// The tripwire fake's model was called exactly once — round 2. Round 1 was
	// replayed from the journal, never re-sampled (else the tripwire would have
	// errored the resume above).
	resumeCalls := llm2.ToolLoopCalls()
	if len(resumeCalls) != 1 {
		t.Fatalf("resume model calls = %d, want 1 (round 1 replayed, round 2 sampled)", len(resumeCalls))
	}

	// The round-2 invocation's Messages replay round-1's history with matching
	// tool_call_ids: [user, assistant(tool_calls c1), tool(c1), ...].
	msgs := resumeCalls[0].Messages
	if len(msgs) < 3 {
		t.Fatalf("round-2 replayed history len = %d, want >=3 (user + assistant + tool): %+v", len(msgs), msgs)
	}
	// Resume-determinism: the initial user turn is RE-TEMPLATED on resume (against
	// input restored from run.started's InputRef) and must produce the byte-
	// identical substituted string the fresh run produced.
	if msgs[0].Role != "user" || msgs[0].Content != reactPromptExpanded {
		t.Errorf("replayed msgs[0] = %+v, want user turn %q (prompt re-templated identically on resume)", msgs[0], reactPromptExpanded)
	}
	assistant := msgs[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "c1" {
		t.Fatalf("replayed msgs[1] = %+v, want assistant with tool_call id c1", assistant)
	}
	toolMsg := msgs[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "c1" {
		t.Fatalf("replayed msgs[2] = %+v, want tool turn with tool_call_id c1", toolMsg)
	}
	// THE ID-equality invariant: assistant.tool_calls[*].id == tool.tool_call_id.
	if assistant.ToolCalls[0].ID != toolMsg.ToolCallID {
		t.Errorf("tool_call_id mismatch: assistant=%q tool=%q (must be equal on replay)",
			assistant.ToolCalls[0].ID, toolMsg.ToolCallID)
	}

	// The terminal committed on resume with the typed answer.
	postRS, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold post-resume: %v", ferr)
	}
	nr, ok := postRS.LookupCompleted("react[0]")
	if !ok || nr.Outputs["answer"] != "validated" || nr.Outputs["stop_reason"] != "stop" {
		t.Fatalf("post-resume terminal = %v (ok=%v)", nr.Outputs, ok)
	}
}

// testReactResumeTornFrontier (Task 5.3b): crash AFTER round 1's .model leaf
// commits but BEFORE its tool-0 leaf — a torn frontier with no round marker.
// Resume must re-run ONLY the uncommitted tool (the committed model leaf is
// replayed, never re-sampled). Crash ledger:
//
//	k=0 run.started
//	k=1 node.started react[0]
//	k=2 node.completed round-1.model     ← committed
//	k=3 node.completed round-1.tool-0    ← FailAppendAfterN(3) crashes HERE
//
// so the model leaf is durable but the tool leaf and the round marker are not.
func testReactResumeTornFrontier(t *testing.T, factory BackendFactory) {
	t.Helper()

	round1 := agent.ToolLoopResult{FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{
		{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE89"}`},
	}}
	round2 := agent.ToolLoopResult{FinishReason: "stop", Output: map[string]any{"answer": "validated"}, Text: `{"answer":"validated"}`}

	var llm1 *fake.Fake
	register1 := registerReactLLM(t, &llm1, nil, map[int]agent.ToolLoopResult{0: round1, 1: round2})
	h := newHarnessWithAgentRegistry(t, programReactTool(factory), reactToolLoopWorkflow, register1)
	h.input = reactPromptInput // first run binds input; resume restores it from run.started's InputRef

	h.log.FailAppendAfterN(3) // crash at round-1's tool-0 commit (model already durable)
	oc1, err1 := h.runWorkflow(t)
	if err1 == nil || oc1 == engine.OutcomeOK {
		t.Fatalf("first run: oc=%q err=%v, want a crash before the tool leaf commits", oc1, err1)
	}

	// Pre-resume: the model leaf is durable but the tool leaf and the round marker
	// are NOT (torn frontier → startK still 1).
	preRS, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold pre-resume: %v", ferr)
	}
	if _, ok := preRS.LookupCompleted("react[0].round-1.model"); !ok {
		t.Fatalf("pre-resume: round-1.model leaf missing (should be durable)")
	}
	if _, ok := preRS.LookupCompleted("react[0].round-1.tool-0"); ok {
		t.Fatalf("pre-resume: round-1.tool-0 leaf committed, want absent (torn frontier)")
	}
	if got := len(preRS.LookupReactRounds("react[0]")); got != 0 {
		t.Fatalf("pre-resume rounds = %d, want 0 (no marker — torn frontier)", got)
	}

	// Resume: a tripwire fake with committedRounds=0 (no round committed) is wrong
	// here — round 1's model IS committed but its round marker is not, so startK is
	// still 1 and the engine MUST replay the committed model leaf without sampling.
	// A fresh fake whose ONLY scripted call is round 2 (index 1) proves it: if the
	// engine re-sampled the committed round-1 model, it would consume index 0
	// (unscripted → error). We script index 1 only.
	h.log.ClearFault()
	var llm2 *fake.Fake
	register2 := registerReactLLM(t, &llm2,
		func(f *fake.Fake) *fake.Fake { return f.WithToolLoopTripwire(1) },
		map[int]agent.ToolLoopResult{1: round2})
	h.agentRegistry = &agent.Registry{}
	register2(h.agentRegistry)

	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil || oc2 != engine.OutcomeOK {
		t.Fatalf("resume: oc=%q err=%v, want ok", oc2, err2)
	}

	// Round 1's model was NOT re-sampled — the resume fake's only call is round 2.
	if got := len(llm2.ToolLoopCalls()); got != 1 {
		t.Fatalf("resume model calls = %d, want 1 (committed model leaf replays, round 2 samples)", got)
	}

	postRS, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold post-resume: %v", ferr)
	}
	// The uncommitted tool got re-run and committed fresh on resume.
	if _, ok := postRS.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Fatalf("post-resume: round-1.tool-0 leaf missing (uncommitted tool must re-run)")
	}
	// Round 1 now closed; terminal committed.
	if got := len(postRS.LookupReactRounds("react[0]")); got != 1 {
		t.Fatalf("post-resume rounds = %d, want 1 (round 1 closed on resume)", got)
	}
	if _, ok := postRS.LookupCompleted("react[0]"); !ok {
		t.Fatalf("post-resume: terminal react[0] not committed")
	}
}

// testReactTwoSameToolInRound (Task 5.3c): one model round requests the SAME
// tool twice (distinct Index slots 0 and 1). The two dispatches must commit to
// DISTINCT leaves react[0].round-1.tool-0 and react[0].round-1.tool-1 (per-call
// keying on tc.Index), and the next round's history carries both tool results
// with their respective tool_call_ids.
func testReactTwoSameToolInRound(t *testing.T, factory BackendFactory) {
	t.Helper()

	var llm *fake.Fake
	register := registerReactLLM(t, &llm, nil, map[int]agent.ToolLoopResult{
		0: {FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{
			{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE01"}`},
			{Index: 1, ID: "c2", Name: "check", Arguments: `{"iban":"DE02"}`},
		}},
		1: {FinishReason: "stop", Output: map[string]any{"answer": "validated"}, Text: `{"answer":"validated"}`},
	})
	h := newHarnessWithAgentRegistry(t, programReactTool(factory), reactToolLoopWorkflow, register)
	h.input = reactPromptInput

	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("run: oc=%q err=%v", oc, err)
	}

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold: %v", ferr)
	}
	// Two DISTINCT tool leaves for the two same-named calls.
	if _, ok := rs.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Errorf("react[0].round-1.tool-0 leaf missing")
	}
	if _, ok := rs.LookupCompleted("react[0].round-1.tool-1"); !ok {
		t.Errorf("react[0].round-1.tool-1 leaf missing")
	}

	// Round 2's history carries both tool turns with their respective ids.
	calls := llm.ToolLoopCalls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	msgs := calls[1].Messages
	// [user, assistant(2 tool_calls), tool(c1), tool(c2)]
	if len(msgs) != 4 {
		t.Fatalf("round-2 history len = %d, want 4: %+v", len(msgs), msgs)
	}
	if len(msgs[1].ToolCalls) != 2 {
		t.Fatalf("assistant turn tool_calls = %d, want 2", len(msgs[1].ToolCalls))
	}
	gotIDs := []string{msgs[2].ToolCallID, msgs[3].ToolCallID}
	wantIDs := []string{"c1", "c2"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("tool message ids = %v, want %v (per-call keying preserves order)", gotIDs, wantIDs)
	}
}

// testReactViaAgentsRole (Task 5.4): the react: loop must resolve its model
// adapter through a *agent.DerivedAdapter (the agents:-role binding), proving
// engine.toolLoopRunnerFor's interface assertion works through the role wrapper
// — i.e. RunToolLoop is forwarded, NOT erased by the concrete-type check.
//
// Conformance form (not a focused engine unit test): the ir.Validate AWF1057
// gate hard-requires the LITERAL `with.uses: awf/llm`, so a workflow naming a
// role under react.with.uses fails load-time validation (the role-alias case is
// gated authoritatively at run-start by the Caps.Containerless+Threaded
// assertion, but the static literal check still fires — see
// ir/validate_tools.go). We therefore keep the workflow's uses: awf/llm
// (validate-clean) and register a DerivedAdapter UNDER the registry key awf/llm
// (Ref()==roleName), wrapping a distinct fake base. The engine's unchanged
// resolver.Lookup("awf/llm") finds the DerivedAdapter; its RunToolLoop forwards
// to the base. If the engine assumed a concrete *fake.Fake (type erasure) this
// would fail to resolve a runner; that it runs proves the interface path (C2).
func testReactViaAgentsRole(t *testing.T, factory BackendFactory) {
	t.Helper()

	var base *fake.Fake
	register := func(reg *agent.Registry) {
		// The base fake is registered under a DISTINCT ref so the DerivedAdapter
		// (Ref()==awf/llm) does not collide with it. Caps come from the base
		// (DerivedAdapter.Capabilities delegates), so the base must be
		// Containerless+Threaded for the react gate to pass.
		base = fake.New("vendor/llm-base").WithCaps(agent.Caps{Containerless: true, Threaded: true}).
			ScriptToolLoop(0, agent.ToolLoopResult{FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{
				{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE89"}`},
			}}).
			ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Output: map[string]any{"answer": "validated"}, Text: `{"answer":"validated"}`})
		if err := reg.Register(base); err != nil {
			t.Fatalf("register base: %v", err)
		}
		// Bind the base under the role name "awf/llm" (the conformance equivalent
		// of cli/registerRoles' DerivedAdapter wiring). roleWith carries an opaque
		// model default that the step's with: (model: m) overlays — key-blind merge.
		role := agent.NewDerivedAdapter("awf/llm", base, ir.RawConfig{"model": "role-default"})
		if err := reg.Register(role); err != nil {
			t.Fatalf("register role: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, programReactTool(factory), reactToolLoopWorkflow, register)
	h.input = reactPromptInput

	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("run via role: oc=%q err=%v", oc, err)
	}

	// The loop ran THROUGH the DerivedAdapter to the base: the base fake recorded
	// both ToolLoop invocations (forwarded), and the base saw the step with:
	// overlaid on the role with: (model:m wins over role-default).
	calls := base.ToolLoopCalls()
	if len(calls) != 2 {
		t.Fatalf("base ToolLoop calls = %d, want 2 (DerivedAdapter must forward both rounds)", len(calls))
	}
	if got := calls[0].With["model"]; got != "m" {
		t.Errorf("forwarded with[model] = %v, want %q (step with overlays role with)", got, "m")
	}

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("react[0]")
	if !ok || nr.Outputs["answer"] != "validated" {
		t.Fatalf("terminal via role = %v (ok=%v)", nr.Outputs, ok)
	}
}
