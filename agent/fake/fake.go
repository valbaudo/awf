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

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

const defaultVersion = "fake-v1"

// Result is one scripted Launch outcome — the typed Output the fake's Launch
// returns, optional Events emitted on the channel before the result, and
// optional Cost the fake stamps into AgentResult.Metrics.
type Result struct {
	Output map[string]any
	Events []agent.AgentEvent
	Cost   float64 // dollars; stamped into AgentResult.Metrics.Cost.USD
	Tokens agent.MetricTokens
	Files  map[string][]byte
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
}

// New mints a Fake for the given ref. Default Capabilities() returns
// {NativeSchema: true} — matching what the real Claude Code adapter declares
// in slice 5.3. Override with WithCaps for Bucket 15 layer-2 tests.
func New(ref string) *Fake {
	return &Fake{
		ref:     ref,
		version: defaultVersion,
		caps:    agent.Caps{NativeSchema: true},
		scripts: map[int]Result{},
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

// Launch returns the scripted result for the current invocation index.
// Increments the index after the call. Emits each scripted Event on the
// returned channel, then closes the channel, then returns AgentResult
// synchronously — matching the agent.Adapter contract.
func (f *Fake) Launch(_ context.Context, _ container.Handle, inv agent.AgentInvocation) (agent.AgentResult, <-chan agent.AgentEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Record the invocation BEFORE the script check so out-of-bounds tests
	// can still observe what was attempted.
	f.calls = append(f.calls, inv)

	i := f.idx
	f.idx++
	r, ok := f.scripts[i]
	if !ok {
		return agent.AgentResult{}, nil, fmt.Errorf("agent/fake: no scripted result for invocation index %d (ref %q)", i, f.ref)
	}

	// Buffered channel sized for all scripted events so the synchronous
	// path doesn't deadlock if the caller doesn't drain immediately.
	ch := make(chan agent.AgentEvent, len(r.Events))
	for _, ev := range r.Events {
		ch <- ev
	}
	close(ch)

	return agent.AgentResult{
		Output:   r.Output,
		ExitCode: 0,
		Metrics: agent.MetricSet{
			Cost:   agent.MetricCost{USD: r.Cost, Source: "reported"},
			Tokens: r.Tokens,
		},
		Files: r.Files,
	}, ch, nil
}
