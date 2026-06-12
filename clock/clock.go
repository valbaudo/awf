// Package clock provides the Clock and IDGen interfaces, injected wherever time/ids are needed.
package clock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Clock supplies the current time and a deterministic sleep. Injected so tests
// can control both — engine logic NEVER reaches time.Now / time.Sleep directly
// (CLAUDE.md determinism invariant).
type Clock interface {
	Now() time.Time
	// Sleep blocks for d, returning early with ctx.Err() if ctx is cancelled
	// or its deadline expires. d <= 0 returns immediately with nil (or
	// ctx.Err() if already cancelled).
	Sleep(ctx context.Context, d time.Duration) error
}

// IDGen mints the run id — the only nondeterministic id in the runtime.
type IDGen interface {
	NewRunID() string
}

// System is the production Clock (UTC wall-clock + real timer-based Sleep).
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Sleep blocks for d via time.NewTimer + ctx.Done() select. Under
// testing/synctest, time.NewTimer is faked, so this method is the integration
// point for synctest-based timing tests.
func (System) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runIDBytes is the byte length of a freshly minted run id (128 bits).
// runIDHexLen is the resulting hex-encoded string length, exported (lower-case, package-internal)
// for the test that asserts production IDs are the right shape.
const (
	runIDBytes  = 16
	runIDHexLen = 2 * runIDBytes
)

// CryptoIDGen is the production IDGen (128-bit crypto/rand, hex-encoded).
type CryptoIDGen struct{}

func (CryptoIDGen) NewRunID() string {
	var b [runIDBytes]byte
	// On Go 1.20+ crypto/rand.Read does not return errors — the stdlib crashes the process if the
	// OS RNG is unavailable. The branch below is retained defensively (and satisfies errcheck); if
	// a future runtime ever surfaces an error here, the panic gives a clear, attributed message
	// instead of silently corrupting a run.
	if _, err := rand.Read(b[:]); err != nil {
		panic("clock: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Fake is a deterministic Clock+IDGen for tests. NewRunID returns IDs in order.
//
// Fake is safe for concurrent use. The production System clock's Now/Sleep are
// goroutine-safe (time.Now / time.NewTimer), so the Clock contract permits
// concurrent callers — e.g. engine/agent_event_sink.go drives Sleep from
// per-delta-timer goroutines. mu guards the mutable T and i fields; construct
// Fake by named fields only (the zero-value mu is ready to use), and always use
// it via *Fake so the mutex is never copied.
type Fake struct {
	mu  sync.Mutex
	T   time.Time
	IDs []string
	i   int
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.T
}

func (f *Fake) NewRunID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.i >= len(f.IDs) {
		panic(fmt.Sprintf("clock.Fake: ran out of seeded IDs (seeded %d, call %d)", len(f.IDs), f.i+1))
	}
	id := f.IDs[f.i]
	f.i++
	return id
}

// Advance steps the fake clock forward by d. Slice 2.4 (retry / timeout tests) uses
// this to simulate elapsed time without real-time sleeps. Independent of
// testing/synctest — Fake.Now() returns f.T regardless of any synctest bubble's
// faked time.Now(). Whether retry tests use this method or synctest's time-fake
// is slice 2.4's decision; 2.1 ships only the explicit-control primitive.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanceLocked(d)
}

// advanceLocked steps T forward by d; the caller must hold f.mu. It exists so
// Sleep can advance under a single lock acquisition — sync.Mutex is not
// reentrant, so a Sleep that called the public Advance would self-deadlock.
func (f *Fake) advanceLocked(d time.Duration) { f.T = f.T.Add(d) }

// Sleep is the Fake's non-blocking deterministic sleeper — advances T by d (if
// the context is live) and returns immediately. Tests that drive retry
// orchestration use this; the post-loop f.Now() reads as if d had elapsed.
// A pre-cancelled or expired ctx returns ctx.Err() WITHOUT advancing T.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		f.mu.Lock()
		f.advanceLocked(d)
		f.mu.Unlock()
	}
	return nil
}
