package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func TestFake_RefAndCapabilities(t *testing.T) {
	f := fake.New("anthropic/claude-code")
	if f.Ref() != "anthropic/claude-code" {
		t.Errorf("Ref = %q, want %q", f.Ref(), "anthropic/claude-code")
	}
	// Default: NativeSchema true (matches what the real Claude Code adapter declares).
	caps := f.Capabilities()
	if !caps.NativeSchema {
		t.Errorf("default Capabilities().NativeSchema = false, want true")
	}
}

func TestFake_Version_DefaultAndOverride(t *testing.T) {
	f := fake.New("anthropic/claude-code")
	v, err := f.Version(context.Background(), container.Handle{Name: "lab"})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v == "" {
		t.Errorf("Version returned empty string; want a default constant")
	}
	f2 := fake.New("anthropic/claude-code").WithVersion("2.1.118")
	v2, _ := f2.Version(context.Background(), container.Handle{Name: "lab"})
	if v2 != "2.1.118" {
		t.Errorf("WithVersion override: got %q, want %q", v2, "2.1.118")
	}
}

func TestFake_ScriptAndLaunch(t *testing.T) {
	f := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"verdict": "pass"}, Cost: 0.01}).
		Script(1, fake.Result{Output: map[string]any{"verdict": "fail"}, Cost: 0.02})

	// First Launch consumes index 0.
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	// Drain events (γ contract: caller must drain before reading outcome).
	count := 0
	for range events {
		count++
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("Launch[0] outcome.Err = %v", outcome.Err)
	}
	r := outcome.Result
	if r.Output["verdict"] != "pass" {
		t.Errorf("Launch[0].Output[verdict] = %v, want %q", r.Output["verdict"], "pass")
	}
	if r.Metrics.Cost.Total != 0.01 {
		t.Errorf("Launch[0].Metrics.Cost.Total = %v, want 0.01", r.Metrics.Cost.Total)
	}
	_ = count // events not asserted in this test (next test covers it)

	// Second Launch consumes index 1.
	events2, outcomeCh2, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[1]: %v", err)
	}
	for range events2 {
	}
	outcome2 := <-outcomeCh2
	if outcome2.Err != nil {
		t.Fatalf("Launch[1] outcome.Err = %v", outcome2.Err)
	}
	if outcome2.Result.Output["verdict"] != "fail" {
		t.Errorf("Launch[1].Output[verdict] = %v, want %q", outcome2.Result.Output["verdict"], "fail")
	}
}

func TestFake_LaunchEmitsScriptedEvents(t *testing.T) {
	f := fake.New("anthropic/claude-code").
		Script(0, fake.Result{
			Output: map[string]any{"ok": true},
			Events: []agent.AgentEvent{
				{Kind: "system", Payload: []byte(`{"subtype":"init"}`)},
				{Kind: "assistant", Payload: []byte(`{"text":"hello"}`)},
				{Kind: "result", Payload: []byte(`{"subtype":"success"}`)},
			},
		})
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var got []string
	for ev := range events {
		got = append(got, ev.Kind)
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	want := []string{"system", "assistant", "result"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d (%v)", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("event[%d].Kind = %q, want %q", i, got[i], k)
		}
	}
}

func TestFake_LaunchOutOfBounds(t *testing.T) {
	f := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"x": 1}})
	// First Launch consumes index 0; second has nothing scripted.
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	for range events {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("Launch[0] outcome.Err = %v", outcome.Err)
	}
	// γ contract: missing script surfaces via outcome.Err, NOT the pre-launch error.
	events2, outcomeCh2, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[1] pre-launch err = %v; want nil (failure surfaces via outcome.Err)", err)
	}
	for range events2 {
	}
	if outcome := <-outcomeCh2; outcome.Err == nil {
		t.Fatalf("Launch[1] outcome.Err = nil, want non-nil for missing script")
	}
}

func TestFake_ValidateConfigDefaultsPermissive(t *testing.T) {
	// Default fake accepts any with-config — tests against the spec's permissive
	// fake convention. Slice 5.3's real claude adapter is strict; the fake stays
	// permissive so test fixtures don't have to mirror the real schema.
	f := fake.New("anthropic/claude-code")
	if err := f.ValidateConfig(ir.RawConfig{"anything": 42, "goes": true}); err != nil {
		t.Errorf("default ValidateConfig rejected arbitrary keys: %v", err)
	}
}

func TestFake_RecordsLaunches(t *testing.T) {
	// Slice 5.2 dispatcher tests need to inspect what was passed to Launch
	// (e.g., to assert gate feedback was templated into With.prompt on attempt 2).
	// The fake records every invocation for test inspection.
	f := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"ok": 1}})
	inv := agent.AgentInvocation{Uses: "anthropic/claude-code", With: map[string]any{"prompt": "first"}}
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range events {
	}
	<-outcomeCh
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1", len(calls))
	}
	if got := calls[0].With["prompt"]; got != "first" {
		t.Errorf("Calls[0].With[prompt] = %v, want %q", got, "first")
	}
}

// Compile-time check: *Fake satisfies agent.Adapter.
var _ agent.Adapter = (*fake.Fake)(nil)

// Sanity: errors from the fake survive errors.Is/errors.As paths.
var _ error = (*agent.ErrAdapterNotFound)(nil)

// Compile-time silence for goimports — keep the errors import alive.
var _ = errors.New

func TestFake_NewBenignOracle_TwoAttempts(t *testing.T) {
	o := fake.NewBenignOracle()
	// Attempt 0 — fake exploit, oracle catches it.
	events0, outcomeCh0, err := o.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "test/oracle"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	for range events0 {
	}
	outcome0 := <-outcomeCh0
	if outcome0.Err != nil {
		t.Fatalf("Launch[0] outcome.Err = %v", outcome0.Err)
	}
	r0 := outcome0.Result
	if r0.Output["verified"] != false {
		t.Errorf("attempt 0: verified = %v, want false", r0.Output["verified"])
	}
	if r0.Output["fooled_by_benign"] != true {
		t.Errorf("attempt 0: fooled_by_benign = %v, want true", r0.Output["fooled_by_benign"])
	}
	// Attempt 1 — real exploit, oracle passes.
	events1, outcomeCh1, err := o.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "test/oracle"})
	if err != nil {
		t.Fatalf("Launch[1]: %v", err)
	}
	for range events1 {
	}
	outcome1 := <-outcomeCh1
	if outcome1.Err != nil {
		t.Fatalf("Launch[1] outcome.Err = %v", outcome1.Err)
	}
	r1 := outcome1.Result
	if r1.Output["verified"] != true {
		t.Errorf("attempt 1: verified = %v, want true", r1.Output["verified"])
	}
	if r1.Output["fooled_by_benign"] != false {
		t.Errorf("attempt 1: fooled_by_benign = %v, want false", r1.Output["fooled_by_benign"])
	}
}

func TestFake_NewBenignOracle_RefIsTestOracle(t *testing.T) {
	o := fake.NewBenignOracle()
	if o.Ref() != "test/oracle" {
		t.Errorf("Ref = %q, want %q", o.Ref(), "test/oracle")
	}
}

func TestFake_Launch_GammaContract_HappyPath(t *testing.T) {
	f := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"answer": 42},
		Events: []agent.AgentEvent{
			{Kind: "system", Payload: []byte(`{"sub":"init"}`)},
			{Kind: "text", Payload: []byte(`hello`)},
			{Kind: "result", Payload: []byte(`{"sub":"success"}`)},
		},
	})

	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Errorf("events len = %d, want 3", len(got))
	}

	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Errorf("outcome.Err = %v", outcome.Err)
	}
	// Output[answer] is whatever the test put in — int 42. The outcome envelope
	// doesn't JSON-roundtrip in Go memory, so it stays as int.
	if outcome.Result.Output["answer"] != 42 {
		t.Errorf("Output[answer] = %v", outcome.Result.Output["answer"])
	}

	// Second receive from outcomeCh must observe close.
	if _, ok := <-outcomeCh; ok {
		t.Errorf("outcomeCh not closed after delivering value")
	}
}

func TestFake_Launch_EmitDelay_RealtimeProgression(t *testing.T) {
	// Asserts emit-delay actually pauses between events — the realtime test
	// for the Claude adapter relies on this facility working in the fake too.
	const delay = 50 * time.Millisecond
	f := fake.New("anthropic/claude-code").WithEmitDelay(delay).Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{
			{Kind: "a"}, {Kind: "b"}, {Kind: "c"},
		},
	})

	start := time.Now()
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var arrivals []time.Duration
	for range events {
		arrivals = append(arrivals, time.Since(start))
	}
	<-outcomeCh

	if len(arrivals) != 3 {
		t.Fatalf("got %d events", len(arrivals))
	}
	span := arrivals[2] - arrivals[0]
	// Two delays between three events; allow some slack for scheduler.
	if span < delay {
		t.Errorf("first→last span = %v, want ≥ %v (proves progressive emission)", span, delay)
	}
}

func TestFake_Launch_FailureBranch_OutcomeCarriesErr(t *testing.T) {
	// When the script entry isn't found, the fake delivers AgentOutcome{Err: ...}.
	f := fake.New("anthropic/claude-code") // no scripts registered
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch returned pre-launch err = %v; want nil (fake's failure surfaces as outcome.Err)", err)
	}
	for range events { // empty stream
	}
	outcome := <-outcomeCh
	if outcome.Err == nil {
		t.Error("outcome.Err = nil; want non-nil for missing script")
	}
}

func TestFake_Launch_StallUntilCancel(t *testing.T) {
	// Trailing-stall mode: after emitting the scripted Events the fake holds the
	// events channel OPEN (does not close it) until ctx is cancelled, then yields
	// an outcome carrying ctx.Err(). This lets dispatcher idle-timeout tests drive
	// a fake into the idle path — normally the fake closes events immediately and
	// the dispatcher disarms the idle timer on channel close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output:           map[string]any{"ok": true},
		Events:           []agent.AgentEvent{{Kind: "progress"}},
		StallUntilCancel: true,
	})

	events, outcomeCh, err := f.Launch(ctx, container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The one scripted event arrives first.
	ev, ok := <-events
	if !ok {
		t.Fatal("events channel closed before any event; want the scripted event first")
	}
	if ev.Kind != "progress" {
		t.Fatalf("event.Kind = %q, want %q", ev.Kind, "progress")
	}

	// The channel must stay OPEN (blocked) until ctx is cancelled — the trailing stall.
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events channel closed before ctx cancel; trailing stall did not hold it open")
		}
		t.Fatalf("unexpected extra event before cancel: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// Good: still blocked/open.
	}

	// Cancel — the stall releases, closes events, and yields ctx.Err().
	cancel()

	for range events { // drains to close
	}
	outcome := <-outcomeCh
	if !errors.Is(outcome.Err, context.Canceled) {
		t.Fatalf("outcome.Err = %v, want context.Canceled", outcome.Err)
	}
}

func TestFake_Launch_TranscriptRoundTrip(t *testing.T) {
	// Phase 3 Task 3.3: Result.Transcript is copied verbatim into
	// AgentResult.Transcript so conformance tests can assert per-turn content
	// without a real adapter.
	want := agent.ThreadTurn{User: "u0", Assistant: "a0"}
	f := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output:     map[string]any{"ok": true},
		Transcript: want,
	})

	events, outcomeCh, err := f.Launch(
		context.Background(),
		container.Handle{Name: "lab"},
		agent.AgentInvocation{Uses: "anthropic/claude-code"},
	)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range events {
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if got := outcome.Result.Transcript; got != want {
		t.Errorf("Transcript = %+v, want %+v", got, want)
	}
}

func TestFakeStampsReportedSourceOnlyWhenCostNonZero(t *testing.T) {
	// A scripted zero cost must NOT be marked "reported" (no cost was reported).
	fZero := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"ok": true}, Cost: 0})
	evZ, outZ, err := fZero.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch(zero): %v", err)
	}
	for range evZ {
	}
	outcomeZ := <-outZ
	if outcomeZ.Err != nil {
		t.Fatalf("Launch(zero) outcome.Err = %v", outcomeZ.Err)
	}
	if got := outcomeZ.Result.Metrics.Cost.Source; got != "" {
		t.Errorf("zero cost: Metrics.Cost.Source = %q, want %q (empty)", got, "")
	}
	if got := outcomeZ.Result.Metrics.Cost.Total; got != 0 {
		t.Errorf("zero cost: Metrics.Cost.Total = %v, want 0", got)
	}

	// A scripted non-zero cost IS marked "reported".
	fPaid := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"ok": true}, Cost: 0.02})
	evP, outP, err := fPaid.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch(paid): %v", err)
	}
	for range evP {
	}
	outcomeP := <-outP
	if outcomeP.Err != nil {
		t.Fatalf("Launch(paid) outcome.Err = %v", outcomeP.Err)
	}
	if got := outcomeP.Result.Metrics.Cost.Source; got != agent.CostSourceReported {
		t.Errorf("paid cost: Metrics.Cost.Source = %q, want %q", got, agent.CostSourceReported)
	}
	if got := outcomeP.Result.Metrics.Cost.Total; got != 0.02 {
		t.Errorf("paid cost: Metrics.Cost.Total = %v, want 0.02", got)
	}
}

// --- ToolLoopRunner tests (Task 5.1) ---

// TestFakeToolLoopScriptedAndTripwire is the primary TDD test from the plan.
// It verifies: ScriptToolLoop builder, RunToolLoop returns scripted result per
// call index, ToolLoopCalls() records invocations, compile-time interface assertion.
func TestFakeToolLoopScriptedAndTripwire(t *testing.T) {
	f := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		ScriptToolLoop(0, agent.ToolLoopResult{FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "t"}}}).
		ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Text: "done"})
	var _ agent.ToolLoopRunner = f // compile-time assertion

	r0, err := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{NodePath: "react[0].round-1.model"})
	if err != nil {
		t.Fatalf("RunToolLoop[0]: %v", err)
	}
	if r0.FinishReason != "tool_calls" {
		t.Fatalf("call 0 FinishReason = %q, want %q", r0.FinishReason, "tool_calls")
	}
	if len(r0.ToolCalls) != 1 || r0.ToolCalls[0].ID != "c1" {
		t.Fatalf("call 0 ToolCalls = %+v", r0.ToolCalls)
	}

	// ToolLoopCalls() records the invocation.
	calls := f.ToolLoopCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolLoopCalls() len = %d, want 1", len(calls))
	}
	if calls[0].NodePath != "react[0].round-1.model" {
		t.Fatalf("recorded NodePath = %q, want react[0].round-1.model", calls[0].NodePath)
	}

	r1, err := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{NodePath: "react[0].round-2.model"})
	if err != nil {
		t.Fatalf("RunToolLoop[1]: %v", err)
	}
	if r1.FinishReason != "stop" || r1.Text != "done" {
		t.Fatalf("call 1 = %+v", r1)
	}

	// ToolLoopCalls() defensive copy — mutating returned slice must not affect internal state.
	calls2 := f.ToolLoopCalls()
	if len(calls2) != 2 {
		t.Fatalf("ToolLoopCalls() len after 2 calls = %d, want 2", len(calls2))
	}
	calls2[0].NodePath = "tampered"
	calls3 := f.ToolLoopCalls()
	if calls3[0].NodePath == "tampered" {
		t.Fatal("ToolLoopCalls() returned non-defensive copy; internal state was mutated")
	}
}

// TestFakeToolLoop_MissingScript verifies that RunToolLoop returns an error
// (not panic) when no script is registered for the current call index.
func TestFakeToolLoop_MissingScript(t *testing.T) {
	f := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
	// No ScriptToolLoop registered.
	_, err := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{})
	if err == nil {
		t.Fatal("RunToolLoop with no script: expected error, got nil")
	}
}

// TestFakeToolLoop_Tripwire verifies the resume tripwire contract:
// WithToolLoopTripwire(N) advances the internal index to N and hard-fails if
// somehow called for an index in the committed range (< N). In normal sequential
// use, the advancing counter makes it impossible to hit an index < N after the
// tripwire is armed. The over-sampling check (index with no script → error) is
// the primary guard; this test validates the advancing behavior and script routing.
func TestFakeToolLoop_Tripwire(t *testing.T) {
	// committedRounds=1: index advances to 1. Script at index 1 is the resume round.
	// Calling RunToolLoop once must succeed with the scripted result (not tripwire).
	f := fake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		WithToolLoopTripwire(1). // 1 committed round → toolLoopIdx advances to 1
		ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Text: "resume-round"})

	r, err := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{NodePath: "react[0].round-2.model"})
	if err != nil {
		t.Fatalf("RunToolLoop for resume round (index 1) failed: %v", err)
	}
	if r.Text != "resume-round" {
		t.Fatalf("resume round text = %q, want %q", r.Text, "resume-round")
	}

	// Over-sampling: calling again when no script exists at index 2 must hard-fail.
	_, err2 := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{})
	if err2 == nil {
		t.Fatal("over-sampling past scripted rounds must hard-fail; got nil error")
	}
}

// TestFakeToolLoop_TripwireAllowsResumeRound verifies that after tripwiring N rounds,
// RunToolLoop succeeds for index N (the first non-committed round).
func TestFakeToolLoop_TripwireAllowsResumeRound(t *testing.T) {
	f := fake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		WithToolLoopTripwire(1).
		ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Text: "resumed"})

	// Index 1 (the first non-committed round) must succeed.
	r, err := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{NodePath: "react[0].round-2.model"})
	if err != nil {
		t.Fatalf("RunToolLoop for resume index 1 failed: %v", err)
	}
	if r.Text != "resumed" {
		t.Fatalf("resume round text = %q, want %q", r.Text, "resumed")
	}
}

// TestFakeToolLoop_ExistingAdapterBehaviorUnchanged confirms the existing Fake
// agent.Adapter (Launch) behavior is unaffected by the ToolLoopRunner additions.
func TestFakeToolLoop_ExistingAdapterBehaviorUnchanged(t *testing.T) {
	f := fake.New("ref").
		Script(0, fake.Result{Output: map[string]any{"ok": true}}).
		ScriptToolLoop(0, agent.ToolLoopResult{FinishReason: "stop", Text: "loop"})

	// Launch still works with its own script table.
	events, outcomeCh, err := f.Launch(context.Background(), container.Handle{Name: "c"}, agent.AgentInvocation{Uses: "ref"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range events {
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("Launch outcome.Err = %v", outcome.Err)
	}
	if outcome.Result.Output["ok"] != true {
		t.Fatalf("Launch output = %v", outcome.Result.Output)
	}
	// Calls() (for Launch) must not be contaminated by ToolLoopCalls.
	if got := len(f.Calls()); got != 1 {
		t.Fatalf("Calls() len = %d, want 1", got)
	}
	if got := len(f.ToolLoopCalls()); got != 0 {
		t.Fatalf("ToolLoopCalls() should be empty before RunToolLoop; got %d", got)
	}
}

// TestFakeToolLoopSatisfiesInterface is a package-level compile-time assertion.
var _ agent.ToolLoopRunner = (*fake.Fake)(nil)

// TestFakeToolLoopCapsPassThrough verifies that WithCaps sets
// Containerless+Threaded so runReact's gate passes.
func TestFakeToolLoopCapsPassThrough(t *testing.T) {
	f := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
	caps := f.Capabilities()
	if !caps.Containerless || !caps.Threaded {
		t.Fatalf("Capabilities() = %+v; want Containerless=true Threaded=true", caps)
	}
}

// Ensure ir import is used (WithCaps and agent imports are used above).
var _ = ir.RawConfig{}
