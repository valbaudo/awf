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
