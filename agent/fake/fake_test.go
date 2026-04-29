package fake_test

import (
	"context"
	"errors"
	"testing"

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
	r, ch, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	// Channel must be closed (the contract: emit all events, then close).
	count := 0
	for range ch {
		count++
	}
	if r.Output["verdict"] != "pass" {
		t.Errorf("Launch[0].Output[verdict] = %v, want %q", r.Output["verdict"], "pass")
	}
	if r.Metrics.Cost.USD != 0.01 {
		t.Errorf("Launch[0].Metrics.Cost.USD = %v, want 0.01", r.Metrics.Cost.USD)
	}
	_ = count // events not asserted in this test (next test covers it)

	// Second Launch consumes index 1.
	r2, _, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[1]: %v", err)
	}
	if r2.Output["verdict"] != "fail" {
		t.Errorf("Launch[1].Output[verdict] = %v, want %q", r2.Output["verdict"], "fail")
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
	_, ch, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var got []string
	for ev := range ch {
		got = append(got, ev.Kind)
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
	_, _, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	_, _, err = f.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "anthropic/claude-code"})
	if err == nil {
		t.Fatalf("Launch[1] with no script: err = nil, want non-nil")
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
	_, _, err := f.Launch(context.Background(), container.Handle{Name: "lab"}, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
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
	r0, _, err := o.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "test/oracle"})
	if err != nil {
		t.Fatalf("Launch[0]: %v", err)
	}
	if r0.Output["verified"] != false {
		t.Errorf("attempt 0: verified = %v, want false", r0.Output["verified"])
	}
	if r0.Output["fooled_by_benign"] != true {
		t.Errorf("attempt 0: fooled_by_benign = %v, want true", r0.Output["fooled_by_benign"])
	}
	// Attempt 1 — real exploit, oracle passes.
	r1, _, err := o.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{Uses: "test/oracle"})
	if err != nil {
		t.Fatalf("Launch[1]: %v", err)
	}
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
