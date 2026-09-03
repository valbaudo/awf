package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// idleTestAdapter is a minimal containerless agent adapter whose Launch behavior
// is driven by a test-supplied function, so a test can make it stall (emit
// nothing, block until ctx cancel) or stay chatty (emit events over time). It
// exists to exercise runAgent's idle-timeout timer deterministically.
type idleTestAdapter struct {
	run func(ctx context.Context, events chan<- agent.AgentEvent) agent.AgentOutcome
}

func (a *idleTestAdapter) Ref() string              { return "test/idle" }
func (a *idleTestAdapter) Capabilities() agent.Caps { return agent.Caps{Containerless: true} }
func (a *idleTestAdapter) Version(context.Context, container.Handle) (string, error) {
	return "1", nil
}
func (a *idleTestAdapter) ValidateConfig(ir.RawConfig) error { return nil }
func (a *idleTestAdapter) Launch(ctx context.Context, _ container.Handle, _ agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	events := make(chan agent.AgentEvent, 16)
	outcome := make(chan agent.AgentOutcome, 1)
	go func() {
		oc := a.run(ctx, events)
		close(events)
		outcome <- oc
	}()
	return events, outcome, nil
}

// contextIgnoringAdapter violates the adapter lifecycle contract by ignoring
// cancellation and leaving both channels open forever. The dispatcher still
// must honor its own timeout instead of trusting adapter cleanup.
type contextIgnoringAdapter struct{}

func (*contextIgnoringAdapter) Ref() string              { return "test/context-ignoring" }
func (*contextIgnoringAdapter) Capabilities() agent.Caps { return agent.Caps{Containerless: true} }
func (*contextIgnoringAdapter) Version(context.Context, container.Handle) (string, error) {
	return "1", nil
}
func (*contextIgnoringAdapter) ValidateConfig(ir.RawConfig) error { return nil }
func (*contextIgnoringAdapter) Launch(context.Context, container.Handle, agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	return make(chan agent.AgentEvent), make(chan agent.AgentOutcome), nil
}

func runIdleAgent(t *testing.T, ri engine.ResolvedInputs, adapter *idleTestAdapter) engine.DispatchResult {
	t.Helper()
	reg := &agent.Registry{}
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ri.Uses = "test/idle"
	d := &engine.LocalDispatcher{Resolver: reg}
	intent := engine.NodeIntent{
		Path:           "gen",
		Node:           &ir.AgentStep{ID: "gen", Uses: "test/idle"},
		ResolvedInputs: ri,
	}
	dr, ch, err := d.Run(context.Background(), intent)
	for range ch {
	}
	if err != nil {
		t.Fatalf("Run engine-level error: %v", err)
	}
	return dr
}

// TestRunAgent_TimeoutDoesNotWaitForContextIgnoringAdapter verifies that the
// dispatcher's wall timeout remains authoritative even if a broken adapter
// ignores ctx and never closes or sends on either channel.
func TestRunAgent_TimeoutDoesNotWaitForContextIgnoringAdapter(t *testing.T) {
	adapter := &contextIgnoringAdapter{}
	reg := &agent.Registry{}
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{Resolver: reg}
	intent := engine.NodeIntent{
		Path: "gen",
		Node: &ir.AgentStep{ID: "gen", Uses: adapter.Ref()},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:    adapter.Ref(),
			Timeout: 25 * time.Millisecond,
		},
	}

	type runResult struct {
		dr     engine.DispatchResult
		chunks <-chan container.IOChunk
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		dr, chunks, err := d.Run(context.Background(), intent)
		done <- runResult{dr: dr, chunks: chunks, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run engine-level error: %v", got.err)
		}
		if got.dr.Outcome != engine.OutcomeRetryableFailure {
			t.Fatalf("Outcome = %v, want retryable_failure (wall timeout)", got.dr.Outcome)
		}
		if !errors.Is(got.dr.Err, context.DeadlineExceeded) {
			t.Fatalf("dr.Err = %v, want context deadline exceeded", got.dr.Err)
		}
		if got.chunks == nil {
			t.Fatal("chunks = nil, want closed channel")
		}
		if _, ok := <-got.chunks; ok {
			t.Fatal("chunks channel is open, want closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Run remained blocked on adapter channels after its timeout")
	}
}

// TestRunAgent_IdleTimeoutFires: an agent that produces no output blocks until
// the idle timer cancels ctx; the ctx-cancel surfaces as ErrAgentLaunch →
// retryable_failure. Deterministic: the adapter waits for the actual cancel.
func TestRunAgent_IdleTimeoutFires(t *testing.T) {
	adapter := &idleTestAdapter{
		run: func(ctx context.Context, _ chan<- agent.AgentEvent) agent.AgentOutcome {
			<-ctx.Done() // stall until the idle timer cancels us
			return agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
		},
	}
	dr := runIdleAgent(t, engine.ResolvedInputs{IdleTimeout: 50 * time.Millisecond}, adapter)
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %v, want retryable_failure (idle fire)", dr.Outcome)
	}
}

// TestRunAgent_IdleTimeoutResetByEvents: an agent that emits an event every 15ms
// for ~120ms (each gap well under the 80ms idle window, total elapsed above it)
// must NOT be killed — each event resets the idle timer — and completes OK.
func TestRunAgent_IdleTimeoutResetByEvents(t *testing.T) {
	adapter := &idleTestAdapter{
		run: func(ctx context.Context, events chan<- agent.AgentEvent) agent.AgentOutcome {
			for i := 0; i < 8; i++ {
				select {
				case <-ctx.Done():
					return agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
				case <-time.After(15 * time.Millisecond):
				}
				events <- agent.AgentEvent{Kind: "progress"}
			}
			return agent.AgentOutcome{Result: agent.AgentResult{Output: map[string]any{"ok": true}}}
		},
	}
	dr := runIdleAgent(t, engine.ResolvedInputs{IdleTimeout: 80 * time.Millisecond}, adapter)
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok (events kept resetting the idle timer)", dr.Outcome)
	}
}

// TestRunAgent_NoIdleTimeout_CompletesNormally: with IdleTimeout unset a normal
// step is unaffected (guards against the timer arming spuriously).
func TestRunAgent_NoIdleTimeout_CompletesNormally(t *testing.T) {
	adapter := &idleTestAdapter{
		run: func(_ context.Context, _ chan<- agent.AgentEvent) agent.AgentOutcome {
			return agent.AgentOutcome{Result: agent.AgentResult{Output: map[string]any{"ok": true}}}
		},
	}
	dr := runIdleAgent(t, engine.ResolvedInputs{}, adapter)
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", dr.Outcome)
	}
}

// stallBackend embeds the fake backend and overrides Exec to produce no output
// and block until ctx is cancelled — a stalled code step.
type stallBackend struct{ *container.Fake }

func (b *stallBackend) Exec(ctx context.Context, _ container.Handle, _ container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	chunks := make(chan container.IOChunk)
	result := make(chan container.ExecResult, 1)
	go func() {
		<-ctx.Done() // stall until the idle timer cancels us
		close(chunks)
		result <- container.ExecResult{Err: ctx.Err()}
	}()
	return chunks, result, nil
}

// TestRunCode_IdleTimeoutFires exercises the code-step idle path (runCode): a
// step producing no IOChunk for the idle window is cancelled → retryable_failure.
func TestRunCode_IdleTimeoutFires(t *testing.T) {
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend: &stallBackend{Fake: fake},
		Handles: map[string]container.Handle{"lab": h},
	}
	intent := engine.NodeIntent{
		Path:           "step",
		Node:           &ir.CodeStep{ID: "step", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{Command: "sleep 999", IdleTimeout: 50 * time.Millisecond},
	}
	dr, ch, err := d.Run(context.Background(), intent)
	if ch != nil { // runCode returns a nil chunk channel on the transport-error path
		for range ch {
		}
	}
	if err != nil {
		t.Fatalf("Run engine-level error: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %v, want retryable_failure (code-step idle fire)", dr.Outcome)
	}
}
