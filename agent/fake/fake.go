// Package fake provides an in-memory scripted agent.Adapter implementation.
// Used by Phase 5 conformance tests (Buckets 12/13/15) and by slice 5.2's
// dispatcher unit tests. Mirrors the container/fake pattern — scripted-
// result table keyed on the invocation index, plus a Calls() history for
// dispatcher-side assertions (e.g., gate feedback substitution on attempt 2).
//
// Phase 5 design decision 10: builder API. fake.New(ref).Script(0, ...).
// Script(1, ...).WithCaps(...).WithTranscriptExtractor(...) chains, returning
// the same *Fake. Goroutine-safe via sync.Mutex.
//
// The fake honors agent.Adapter's streaming contract: Launch emits each
// scripted AgentEvent on the returned channel, closes the channel, and
// returns AgentResult synchronously. Slice 5.2 dispatcher tests range over
// the channel and assert event ordering.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

const defaultVersion = "fake-v1"

// Result is one scripted Launch outcome — the typed Output the fake's Launch
// returns, optional Events emitted on the channel before the result, and
// optional Cost the fake stamps into AgentResult.Metrics.
type Result struct {
	Output     map[string]any
	Events     []agent.AgentEvent
	Cost       float64 // dollars; stamped into AgentResult.Metrics.Cost.Total
	Tokens     agent.MetricTokens
	Files      map[string][]byte
	Live       *agent.LiveDispatch
	Transcript agent.ThreadTurn // scriptable verbatim pair; copied into AgentResult.Transcript by Launch

	// Err, when non-nil, makes Launch emit it as AgentOutcome.Err (after any
	// scripted Events) instead of a success AgentResult — lets tests script
	// transient/permanent launch failures (e.g. *agent.ErrAgentLaunch with a
	// RetryHint) and drive the dispatcher's failure classification.
	Err error

	// StallUntilCancel, when true, makes the emitter block AFTER emitting the
	// scripted Events instead of finishing: it holds the events channel OPEN
	// (does not close it) until ctx is cancelled, then yields an outcome carrying
	// ctx.Err(). Without this, the fake closes events immediately and the
	// dispatcher disarms its idle timer on channel close — so idle can never fire
	// in a fake test. This trailing-stall mode lets idle-timeout tests run against
	// the fake backend (mirrors the bespoke idleTestAdapter in engine tests).
	StallUntilCancel bool
}

// Fake is the in-memory scripted adapter. Zero value is NOT usable — call
// New(ref) so Ref() returns a non-empty string.
type Fake struct {
	mu sync.Mutex

	ref       string
	version   string
	caps      agent.Caps
	scripts   map[int]Result
	extractor func(transcript string) (map[string]any, error)
	calls     []agent.AgentInvocation
	idx       int
	emitDelay time.Duration

	// ToolLoopRunner fields (P3 Task 5.1). Additive — Launch/Script/Calls are unchanged.
	toolLoopScripts  map[int]agent.ToolLoopResult
	toolLoopCalls    []agent.ToolLoopInvocation
	toolLoopIdx      int
	toolLoopTripwire int // >0 means indices [0..tripwire-1] must NOT be called (hard-fail)
}

// compile-time assertion: *Fake satisfies agent.ToolLoopRunner.
var _ agent.ToolLoopRunner = (*Fake)(nil)

// New mints a Fake for the given ref. Default Capabilities() returns
// {NativeSchema: true} — matching what the real Claude Code adapter declares
// in slice 5.3. Override with WithCaps for Bucket 15 layer-2 tests.
func New(ref string) *Fake {
	return &Fake{
		ref:             ref,
		version:         defaultVersion,
		caps:            agent.Caps{NativeSchema: true},
		scripts:         map[int]Result{},
		toolLoopScripts: map[int]agent.ToolLoopResult{},
	}
}

// Ref returns the adapter ref this fake was constructed with.
func (f *Fake) Ref() string { return f.ref }

// Capabilities returns the static caps (override with WithCaps).
func (f *Fake) Capabilities() agent.Caps { return f.caps }

// Version returns the fake's version string. Default is "fake-v1"; override
// with WithVersion for drift-test fixtures.
func (f *Fake) Version(_ context.Context, _ container.Handle) (string, error) {
	return f.version, nil
}

// ValidateConfig is permissive by default — accepts any RawConfig. Slice
// 5.3's real claude adapter is strict; fixtures targeting the fake don't
// have to mirror the real schema.
func (*Fake) ValidateConfig(_ ir.RawConfig) error { return nil }

// Script programs the result for the Nth Launch call (zero-indexed).
// Returns *Fake for chaining. Calling Script twice on the same index
// silently overwrites — the latest wins, matching container/fake's
// ProgramExec convention.
func (f *Fake) Script(n int, r Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[n] = r
	return f
}

// WithCaps overrides the default Capabilities(). Used by Bucket 15 tests
// to construct a `Caps{NativeSchema: false}` fake.
func (f *Fake) WithCaps(c agent.Caps) *Fake {
	f.caps = c
	return f
}

// WithVersion overrides the default Version() return value. Used by the
// drift-test fixture in cli/resume_test.go.
func (f *Fake) WithVersion(v string) *Fake {
	f.version = v
	return f
}

// WithEmitDelay sets the inter-event delay used by Launch's emitter
// goroutine. Realtime tests (TestClaudeAdapterRealtimeStreaming and the
// fake's own TestFake_Launch_EmitDelay_RealtimeProgression) use this to
// produce verifiable wall-clock progression. Default 0 (no delay).
func (f *Fake) WithEmitDelay(d time.Duration) *Fake {
	f.emitDelay = d
	return f
}

// WithTranscriptExtractor sets the layer-2 extractor closure. The fake
// itself doesn't use it in Launch (Launch just consults the script table);
// the closure is exposed to test callers via Extractor() so Bucket 15
// tests can verify the contract without coupling the fake to a real layer-2
// implementation.
//
// (Phase 5 design Appendix H: the real structuring-call helper is deferred
// to whichever future non-native-schema adapter ships first. The fake
// simulates the contract via this opt-in closure.)
func (f *Fake) WithTranscriptExtractor(fn func(transcript string) (map[string]any, error)) *Fake {
	f.extractor = fn
	return f
}

// Extractor returns the extractor closure set via WithTranscriptExtractor
// (or nil). Bucket 15 tests use this to drive the layer-2 contract.
func (f *Fake) Extractor() func(transcript string) (map[string]any, error) {
	return f.extractor
}

// Calls returns a defensive copy of every AgentInvocation Launch has
// received. Slice 5.2 dispatcher tests inspect this to assert gate
// feedback substitution and AgentInvocation field threading.
func (f *Fake) Calls() []agent.AgentInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.AgentInvocation, len(f.calls))
	copy(out, f.calls)
	return out
}

// Launch honors the slice 5.3 γ contract: returns IMMEDIATELY with events
// and outcome channels OPEN. An emitter goroutine writes each scripted
// event to the events channel (with optional inter-event delay), closes
// events, then sends AgentOutcome and closes outcomeCh. Caller MUST drain
// events before reading outcome (the standard `for range events; outcome
// := <-outcomeCh` pattern).
func (f *Fake) Launch(ctx context.Context, _ container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	i := f.idx
	f.idx++
	r, ok := f.scripts[i]
	delay := f.emitDelay
	f.mu.Unlock()

	events := make(chan agent.AgentEvent, len(r.Events)+1)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	go func() {
		// defer LIFO means declared-last runs first. We want events to
		// close BEFORE outcomeCh so receivers (and the Adapter contract
		// doc) see the documented order. Declare outcomeCh FIRST (runs
		// last) and events SECOND (runs first). Matches the Claude
		// adapter's defer order in launch.go.
		defer close(outcomeCh)
		defer close(events)

		if !ok {
			outcomeCh <- agent.AgentOutcome{
				Err: fmt.Errorf("agent/fake: no scripted result for invocation index %d (ref %q)", i, f.ref),
			}
			return
		}

		for _, ev := range r.Events {
			select {
			case <-ctx.Done():
				outcomeCh <- agent.AgentOutcome{Err: ctx.Err()}
				return
			case events <- ev:
			}
			if delay > 0 {
				select {
				case <-ctx.Done():
					outcomeCh <- agent.AgentOutcome{Err: ctx.Err()}
					return
				case <-time.After(delay):
				}
			}
		}

		// Trailing-stall mode: hold the events channel OPEN (don't close it via
		// the deferred close until we return) and block until ctx is cancelled,
		// then yield ctx.Err(). Lets idle-timeout tests drive the fake into the
		// idle path — the dispatcher disarms its idle timer on events-channel
		// close, so a fake must stay open past its last event for idle to fire.
		if r.StallUntilCancel {
			<-ctx.Done()
			outcomeCh <- agent.AgentOutcome{Err: ctx.Err()}
			return
		}

		// Scripted launch failure: emit it as the outcome error (after any
		// scripted Events above) instead of a success result.
		if r.Err != nil {
			outcomeCh <- agent.AgentOutcome{Err: r.Err}
			return
		}

		cost := agent.MetricCost{}
		if r.Cost != 0 {
			cost = agent.MetricCost{Total: r.Cost, Source: agent.CostSourceReported}
		}
		outcomeCh <- agent.AgentOutcome{
			Result: agent.AgentResult{
				Output:   r.Output,
				ExitCode: 0,
				Metrics: agent.MetricSet{
					Cost:   cost,
					Tokens: r.Tokens,
				},
				Files:      r.Files,
				Live:       r.Live,
				Transcript: r.Transcript,
			},
		}
	}()

	return events, outcomeCh, nil
}

// --- ToolLoopRunner implementation (P3 Task 5.1) ---

// ScriptToolLoop programs the ToolLoopResult for the Nth RunToolLoop call
// (zero-indexed). Returns *Fake for chaining. Calling ScriptToolLoop twice
// on the same index silently overwrites — the latest wins (mirrors Script).
func (f *Fake) ScriptToolLoop(n int, r agent.ToolLoopResult) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolLoopScripts[n] = r
	return f
}

// WithToolLoopTripwire arms a resume tripwire: if RunToolLoop is invoked for
// any call index < committedRounds, it hard-fails with an error. This proves
// the engine never re-samples committed model rounds on resume (the model-not-
// re-sampled invariant from spec §4.1). committedRounds is the number of rounds
// that were already committed in a prior lifetime (= len(ReactRounds[R])).
//
// The fake's call index is advanced to committedRounds so that ScriptToolLoop
// entries for the resume run can be keyed starting at committedRounds (= the
// first non-committed round index). The tripwire fires if the engine somehow
// calls RunToolLoop for a committed index despite the resume cursor advancing.
func (f *Fake) WithToolLoopTripwire(committedRounds int) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolLoopTripwire = committedRounds
	f.toolLoopIdx = committedRounds // advance past committed rounds
	return f
}

// ToolLoopCalls returns a defensive copy of every ToolLoopInvocation that
// RunToolLoop has received. Resume conformance tests inspect this to assert
// that the round-2 invocation's messages contain the round-1 IDs verbatim.
func (f *Fake) ToolLoopCalls() []agent.ToolLoopInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.ToolLoopInvocation, len(f.toolLoopCalls))
	copy(out, f.toolLoopCalls)
	return out
}

// RunToolLoop satisfies agent.ToolLoopRunner. It is mutex-guarded, returns the
// scripted result for the current call index, and records every invocation.
//
// Tripwire: if WithToolLoopTripwire(N) was set and the current index < N,
// RunToolLoop returns an error immediately — hard-failing to prove the engine
// is not re-sampling a committed model round.
//
// Missing script: if no result was programmed for the current index, RunToolLoop
// returns an error (mirrors Launch's missing-script contract).
func (f *Fake) RunToolLoop(_ context.Context, inv agent.ToolLoopInvocation) (agent.ToolLoopResult, error) {
	f.mu.Lock()
	i := f.toolLoopIdx
	f.toolLoopIdx++
	tripwire := f.toolLoopTripwire
	r, ok := f.toolLoopScripts[i]
	f.toolLoopCalls = append(f.toolLoopCalls, inv)
	f.mu.Unlock()

	// Tripwire check: hard-fail if this index was declared committed.
	if tripwire > 0 && i < tripwire {
		return agent.ToolLoopResult{}, fmt.Errorf(
			"agent/fake: TRIPWIRE — RunToolLoop called for index %d which was committed in a prior lifetime (committedRounds=%d, ref=%q); the engine must not re-sample committed rounds",
			i, tripwire, f.ref,
		)
	}

	if !ok {
		return agent.ToolLoopResult{}, fmt.Errorf(
			"agent/fake: no scripted ToolLoopResult for invocation index %d (ref %q)",
			i, f.ref,
		)
	}
	return r, nil
}
