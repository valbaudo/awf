package agent

import (
	"context"
	"fmt"
	"maps"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// DerivedAdapter is a role-bound view of a base Adapter (C3). It is registered
// under the role name (Ref() == roleName) so the engine's unchanged
// resolver.Lookup(as.Uses) finds it. Every call overlays the step's opaque
// with: ON TOP of the role's with: (step key wins) and forwards to the base —
// the merge is KEY-BLIND (maps.Copy, never inspecting a key), so with:-opacity
// holds: AWF still never interprets a with: field; the named base adapter does.
//
// Independence (spec §5.5) and the no-session contract are the base adapter's
// concern — DerivedAdapter holds no per-call state; the role with: is fixed at
// construction.
type DerivedAdapter struct {
	roleName string
	base     Adapter
	roleWith ir.RawConfig // the role's opaque config (incl. folded model/system_prompt)
}

// NewDerivedAdapter binds base under roleName with roleWith. roleWith is
// defensive-copied; nil is treated as empty.
func NewDerivedAdapter(roleName string, base Adapter, roleWith ir.RawConfig) *DerivedAdapter {
	cp := make(ir.RawConfig, len(roleWith))
	maps.Copy(cp, roleWith)
	return &DerivedAdapter{roleName: roleName, base: base, roleWith: cp}
}

// Ref returns the ROLE name — so the workflow's uses: <role> resolves here and
// run-start Version pinning records (role, container) distinctly from the base.
func (d *DerivedAdapter) Ref() string { return d.roleName }

// Capabilities / Version delegate to the base (the role does not change the
// binary or its typed-output pipeline).
func (d *DerivedAdapter) Capabilities() Caps { return d.base.Capabilities() }
func (d *DerivedAdapter) Version(ctx context.Context, h container.Handle) (string, error) {
	return d.base.Version(ctx, h)
}

// merge overlays step ON TOP of the role (step wins). Key-blind; returns a
// fresh map (never aliases either input).
func (d *DerivedAdapter) merge(step ir.RawConfig) ir.RawConfig {
	out := make(ir.RawConfig, len(d.roleWith)+len(step))
	maps.Copy(out, d.roleWith)
	maps.Copy(out, step) // step wins
	return out
}

func (d *DerivedAdapter) ValidateConfig(with ir.RawConfig) error {
	return d.base.ValidateConfig(d.merge(with))
}

func (d *DerivedAdapter) Launch(ctx context.Context, h container.Handle, inv AgentInvocation) (<-chan AgentEvent, <-chan AgentOutcome, error) {
	inv.With = d.merge(inv.With)
	return d.base.Launch(ctx, h, inv)
}

func (d *DerivedAdapter) PreflightResume(ctx context.Context, req LiveResumePreflightRequest) error {
	preflighter, ok := d.base.(ResumePreflighter)
	if !ok {
		return fmt.Errorf("base adapter %q for role %q declares PersistentSession but does not implement agent.ResumePreflighter", d.base.Ref(), d.roleName)
	}
	req.With = d.merge(req.With)
	return preflighter.PreflightResume(ctx, req)
}

func (d *DerivedAdapter) RunToolLoop(ctx context.Context, inv ToolLoopInvocation) (ToolLoopResult, error) {
	runner, ok := d.base.(ToolLoopRunner)
	if !ok {
		return ToolLoopResult{}, fmt.Errorf("base adapter %q for role %q does not implement agent.ToolLoopRunner", d.base.Ref(), d.roleName)
	}
	inv.With = d.merge(inv.With)
	return runner.RunToolLoop(ctx, inv)
}
