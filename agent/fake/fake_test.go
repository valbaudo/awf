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
	if r.Metrics.Cost.USD != 0.01 {
		t.Errorf("Launch[0].Metrics.Cost.USD = %v, want 0.01", r.Metrics.Cost.USD)
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
