package agent_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// recordingAdapter is a base Adapter that records the with: it last saw in
// ValidateConfig and Launch — the seam to prove DerivedAdapter's key-blind
// overlay (step-wins) reached the base unchanged.
type recordingAdapter struct {
	ref          string
	caps         agent.Caps
	version      string
	versionErr   error
	validateWith ir.RawConfig
	launchWith   ir.RawConfig
}

func (r *recordingAdapter) Ref() string              { return r.ref }
func (r *recordingAdapter) Capabilities() agent.Caps { return r.caps }
func (r *recordingAdapter) Version(context.Context, container.Handle) (string, error) {
	return r.version, r.versionErr
}
func (r *recordingAdapter) ValidateConfig(with ir.RawConfig) error {
	r.validateWith = with
	return nil
}
func (r *recordingAdapter) Launch(_ context.Context, _ container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	r.launchWith = inv.With
	return nil, nil, nil
}

type recordingPreflightAdapter struct {
	recordingAdapter
	preflightReq agent.LiveResumePreflightRequest
	preflightErr error
}

func (r *recordingPreflightAdapter) PreflightResume(_ context.Context, req agent.LiveResumePreflightRequest) error {
	r.preflightReq = req
	return r.preflightErr
}

// Compile-time: DerivedAdapter satisfies Adapter.
var _ agent.Adapter = (*agent.DerivedAdapter)(nil)

func TestDerivedAdapter_OverlayStepWins(t *testing.T) {
	base := &recordingAdapter{ref: "anthropic/claude-code"}
	roleWith := ir.RawConfig{"model": "opus", "mcp_servers": []any{"m"}}
	d := agent.NewDerivedAdapter("auditor", base, roleWith)

	stepWith := ir.RawConfig{"model": "sonnet", "prompt": "p"}
	want := ir.RawConfig{
		"model":       "sonnet",   // step wins over role
		"mcp_servers": []any{"m"}, // role-only (the fleet memory handle)
		"prompt":      "p",        // step-only
	}

	if err := d.ValidateConfig(stepWith); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !reflect.DeepEqual(base.validateWith, want) {
		t.Errorf("ValidateConfig base saw %v, want %v", base.validateWith, want)
	}

	if _, _, err := d.Launch(context.Background(), container.Handle{}, agent.AgentInvocation{With: stepWith}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !reflect.DeepEqual(base.launchWith, want) {
		t.Errorf("Launch base saw %v, want %v", base.launchWith, want)
	}
}

func TestDerivedAdapter_RefIsRoleName(t *testing.T) {
	base := &recordingAdapter{ref: "anthropic/claude-code"}
	d := agent.NewDerivedAdapter("auditor", base, nil)
	if d.Ref() != "auditor" {
		t.Errorf("Ref() = %q, want %q", d.Ref(), "auditor")
	}
}

func TestDerivedAdapter_DelegatesCapabilitiesAndVersion(t *testing.T) {
	wantErr := errors.New("boom")
	base := &recordingAdapter{
		ref:        "anthropic/claude-code",
		caps:       agent.Caps{NativeSchema: true},
		version:    "v1.2.3",
		versionErr: wantErr,
	}
	d := agent.NewDerivedAdapter("auditor", base, nil)

	if got := d.Capabilities(); got != base.caps {
		t.Errorf("Capabilities() = %v, want %v (delegated)", got, base.caps)
	}
	ver, err := d.Version(context.Background(), container.Handle{})
	if ver != "v1.2.3" || !errors.Is(err, wantErr) {
		t.Errorf("Version() = (%q, %v), want (v1.2.3, boom) (delegated)", ver, err)
	}
}

func TestDerivedAdapter_NilRoleWithIsEmpty(t *testing.T) {
	base := &recordingAdapter{ref: "anthropic/claude-code"}
	d := agent.NewDerivedAdapter("auditor", base, nil)
	stepWith := ir.RawConfig{"prompt": "p"}
	if _, _, err := d.Launch(context.Background(), container.Handle{}, agent.AgentInvocation{With: stepWith}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !reflect.DeepEqual(base.launchWith, ir.RawConfig{"prompt": "p"}) {
		t.Errorf("Launch base saw %v, want {prompt:p}", base.launchWith)
	}
}

// The overlay must never alias either input map (key-blind, fresh map).
func TestDerivedAdapter_DoesNotMutateInputs(t *testing.T) {
	base := &recordingAdapter{ref: "anthropic/claude-code"}
	roleWith := ir.RawConfig{"model": "opus"}
	d := agent.NewDerivedAdapter("auditor", base, roleWith)

	stepWith := ir.RawConfig{"model": "sonnet"}
	if _, _, err := d.Launch(context.Background(), container.Handle{}, agent.AgentInvocation{With: stepWith}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Caller's step map untouched.
	if !reflect.DeepEqual(stepWith, ir.RawConfig{"model": "sonnet"}) {
		t.Errorf("step with: mutated to %v", stepWith)
	}
	// Constructor defensive-copies role with: — mutating the original after
	// construction does not leak into the derived adapter.
	roleWith["model"] = "haiku"
	base.launchWith = nil
	if _, _, err := d.Launch(context.Background(), container.Handle{}, agent.AgentInvocation{With: nil}); err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	if base.launchWith["model"] != "opus" {
		t.Errorf("role with: not defensively copied: base saw model=%v, want opus", base.launchWith["model"])
	}
}

func TestDerivedAdapter_PreflightResumeMergesRoleWithAndPreservesRoleRef(t *testing.T) {
	base := &recordingPreflightAdapter{
		recordingAdapter: recordingAdapter{
			ref:  "live/base",
			caps: agent.Caps{PersistentSession: true},
		},
	}
	d := agent.NewDerivedAdapter("auditor", base, ir.RawConfig{
		"model": "opus",
		"cwd":   "/repo",
	})

	req := agent.LiveResumePreflightRequest{
		NodePath:   "gate[0].attempt-1.generate.audit",
		AdapterRef: "auditor",
		With: ir.RawConfig{
			"model":   "sonnet",
			"session": "s1",
		},
		RunID:        "run-1",
		CurrentEpoch: 2,
		NextEpoch:    3,
	}
	if err := d.PreflightResume(context.Background(), req); err != nil {
		t.Fatalf("PreflightResume: %v", err)
	}

	wantWith := ir.RawConfig{
		"model":   "sonnet",
		"cwd":     "/repo",
		"session": "s1",
	}
	if !reflect.DeepEqual(base.preflightReq.With, wantWith) {
		t.Fatalf("base preflight With = %#v, want %#v", base.preflightReq.With, wantWith)
	}
	if base.preflightReq.AdapterRef != "auditor" {
		t.Fatalf("AdapterRef = %q, want role ref auditor", base.preflightReq.AdapterRef)
	}
	if base.preflightReq.RunID != "run-1" || base.preflightReq.CurrentEpoch != 2 || base.preflightReq.NextEpoch != 3 {
		t.Fatalf("run context = %+v, want original request context", base.preflightReq)
	}
}

// stubToolLoopBase embeds recordingAdapter (satisfies agent.Adapter) and adds
// RunToolLoop — making it a base that DerivedAdapter can forward tool-loop calls to.
type stubToolLoopBase struct {
	recordingAdapter
	gotWith agent.ToolLoopInvocation
}

func (s *stubToolLoopBase) RunToolLoop(_ context.Context, inv agent.ToolLoopInvocation) (agent.ToolLoopResult, error) {
	s.gotWith = inv
	return agent.ToolLoopResult{FinishReason: "stop", Text: "ok"}, nil
}

func TestDerivedAdapterForwardsRunToolLoop(t *testing.T) {
	base := &stubToolLoopBase{
		recordingAdapter: recordingAdapter{ref: "awf/llm"},
	}
	d := agent.NewDerivedAdapter("role", base, ir.RawConfig{"model": "role-model"})

	runner, ok := any(d).(agent.ToolLoopRunner)
	if !ok {
		t.Fatal("DerivedAdapter does not satisfy ToolLoopRunner")
	}
	res, err := runner.RunToolLoop(context.Background(), agent.ToolLoopInvocation{With: ir.RawConfig{"prompt": "x"}})
	if err != nil || res.Text != "ok" {
		t.Fatalf("forward failed: %v %v", res, err)
	}
	if base.gotWith.With["model"] != "role-model" {
		t.Fatalf("role with: not merged: %v", base.gotWith.With)
	}
}

// A fake base adapter that records the merged With it was launched with.
type recordingBase struct {
	agent.Adapter
	launched ir.RawConfig
}

func (r *recordingBase) Launch(ctx context.Context, h container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	r.launched = inv.With
	oc := make(chan agent.AgentOutcome, 1)
	oc <- agent.AgentOutcome{Result: agent.AgentResult{}}
	close(oc)
	ev := make(chan agent.AgentEvent)
	close(ev)
	return ev, oc, nil
}

func TestDerivedAdapter_RoleWithOverride_UsedAtLaunch(t *testing.T) {
	base := &recordingBase{}
	d := agent.NewDerivedAdapter("judge", base, ir.RawConfig{"model": "{{ input.model }}"}) // raw
	// Engine supplies the scope-resolved role with:
	inv := agent.AgentInvocation{Uses: "judge", RoleWith: ir.RawConfig{"model": "gpt-5.5"}, With: ir.RawConfig{"prompt": "hi"}}
	_, oc, err := d.Launch(context.Background(), container.Handle{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	<-oc
	if got := base.launched["model"]; got != "gpt-5.5" {
		t.Fatalf("expected resolved role model gpt-5.5, got %v (leaked raw template?)", got)
	}
	if base.launched["prompt"] != "hi" {
		t.Fatalf("step with: lost")
	}
}

func TestDerivedAdapter_RoleWithAccessor_ReturnsRawCopy(t *testing.T) {
	d := agent.NewDerivedAdapter("judge", &recordingBase{}, ir.RawConfig{"model": "{{ input.model }}"})
	got := d.RoleWith()
	if got["model"] != "{{ input.model }}" {
		t.Fatalf("accessor should return raw role with, got %v", got)
	}
	got["model"] = "mutated"
	if d.RoleWith()["model"] != "{{ input.model }}" {
		t.Fatalf("accessor must return a copy, not alias internal state")
	}
}
