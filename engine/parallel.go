package engine

import (
	"context"
	"errors"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// serializingLog wraps a state.Log with a per-instance mutex, serializing
// Append / Sync / Fold / Close. Used inside runParallel to let branch
// goroutines append commits concurrently against a single underlying log
// (which is single-writer per Phase 1.5's contract).
//
// Phase 1's state.InMemoryLog and state.FileLog are NOT modified — the
// wrapper interposes only when the engine is inside a parallel scope.
// Branch commits interleave by Seq through this lock.
//
// Nested parallel: the inner runParallel wraps the already-wrapped log;
// calls go through two locks. Correct (same lock ordering by reference),
// just one extra Lock/Unlock per Append per nesting level.
type serializingLog struct {
	mu    sync.Mutex
	inner state.Log
}

func newSerializingLog(inner state.Log) *serializingLog {
	return &serializingLog{inner: inner}
}

func (s *serializingLog) Append(e state.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Append(e)
}

func (s *serializingLog) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Sync()
}

func (s *serializingLog) Fold() ([]state.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Fold()
}

func (s *serializingLog) Close() error {
	// Defensive symmetry: Close isn't called inside runParallel scope today
	// (the CLI closes logs after engine.Run returns, well after runParallel
	// has terminated). The lock protects against future refactors that
	// might call Close while a cancelled branch is mid-Append.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Close()
}

// serializingWriter wraps an io.Writer with a per-instance mutex so concurrent
// branch goroutines can share a single tap writer (typically os.Stdout, or a
// test's bytes.Buffer) without racing. Each Write is atomic: the prefix +
// chunk produced by drainTap's single fmt.Fprintf call lands as one
// uninterrupted write, so lines from different branches don't interleave
// mid-token.
//
// As with serializingLog, this is interposed only for the duration of
// runParallel — drainTap writes outside any parallel scope go straight to the
// raw tap.
type serializingWriter struct {
	mu    sync.Mutex
	inner io.Writer
}

func newSerializingWriter(inner io.Writer) *serializingWriter {
	return &serializingWriter{inner: inner}
}

func (s *serializingWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Write(p)
}

// runParallel is the Parallel handler (Phase 3 spec §5.4 + design §C).
// Fans children out as concurrent goroutines under a shared ctx derived
// from errgroup.WithContext. First non-skip error from any child cancels
// the shared ctx; siblings observe the cancellation and unwind (their
// enclosing try.Finally blocks run, per slice 3.1 design Q3).
//
// State machine:
//
//  1. Wrap the inbound log in serializingLog so branch goroutines'
//     Appends are serialized through the single-writer underlying log.
//  2. errgroup.WithContext(parent) → group + shared gctx.
//  3. For each child i: g.Go(func() error {
//     oc, err := interpNode(gctx, child, i, parallelPath, ..., wrappedLog, ...)
//     if SkipUnwind: append node.skipped(parallelPath, reason),
//     set outcomes[i] = OutcomeOK, return nil (no group cancel).
//     if err != nil: set outcomes[i], errs[i]; return err (triggers
//     group cancel — siblings observe gctx-cancel).
//     else: set outcomes[i] = OutcomeOK, errs[i] = nil; return nil.
//     })
//  4. g.Wait() — blocks until every child returns. Per Go memory model,
//     Wait synchronizes the goroutines' writes-before-return with our
//     reads-after-Wait. errgroup's race-dependent err is discarded.
//  5. Scan errs[] left-to-right (slice design Q3): lowest-index non-nil
//     error wins (deterministic). Return (outcomes[idx], errs[idx]).
//  6. If no error: return (OutcomeOK, nil).
//
// Validator (Phase 1.4 §5.4 / AWF1010) already enforces distinct
// containers across branches — this handler doesn't re-check.
func runParallel(
	ctx context.Context,
	n *ir.Parallel,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	if len(n.Children) == 0 {
		// Validator should reject; defense-in-depth.
		return OutcomeOK, nil
	}

	wrappedLog := newSerializingLog(log)
	// Same problem the log faces, applied to the live-tap writer: branch
	// goroutines all call drainTap which writes to `tap`. If callers pass a
	// shared writer (os.Stdout or a test buffer) we'd race on the writer's
	// internal state. Wrap it so each fmt.Fprintf lands atomically.
	var wrappedTap io.Writer
	if tap != nil {
		wrappedTap = newSerializingWriter(tap)
	}

	g, gctx := errgroup.WithContext(ctx)
	outcomes := make([]Outcome, len(n.Children))
	errs := make([]error, len(n.Children))

	for i, child := range n.Children {
		i, child := i, child
		g.Go(func() error {
			oc, err := interpNode(gctx, child, i, path, wf, runstate, dispatcher, wrappedLog, blobs, clk, wrappedTap, broker)
			// Skip-in-branch: ends THIS branch as ok, siblings unaffected.
			var su *SkipUnwind
			if errors.As(err, &su) {
				if appErr := appendNodeSkipped(wrappedLog, path, su.Reason); appErr != nil {
					// Log-append failure IS a real failure — cancel siblings.
					errs[i] = appErr
					return appErr
				}
				outcomes[i] = OutcomeOK
				return nil
			}
			outcomes[i] = oc
			errs[i] = err
			return err // nil → no cancel; non-nil → errgroup cancels siblings
		})
	}
	// errgroup.Wait()'s return is the FIRST goroutine to error (race-
	// dependent — non-deterministic). We discard it; the deterministic
	// pick below scans errs[] for the lowest-index non-nil entry.
	_ = g.Wait()

	for i, e := range errs {
		if e != nil {
			return outcomes[i], e
		}
	}
	return OutcomeOK, nil
}
