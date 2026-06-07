package signal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The pause/cancel control surface (PauseRequest/CancelRequest, WritePause/
// WriteCancel, CheckPauseCancel/ClearPauseCancel, and the ErrPaused/ErrCancelled
// sentinels) lives in control.go — same package, same *Broker receiver, same
// controlDir. broker.go is the named-signal queue (WriteSignal/Receive/consume).

// Broker is the per-run control-surface. One Broker per run.id, bound to its
// .awf/runs/<run.id>/control/ directory. The engine constructs an instance at
// run start; the CLI subcommands (awf signal/pause/cancel) construct an
// instance pointing at the same directory to write control files.
//
// Concrete struct (no interface) — see Phase 3 design decision 8 + slice 3.5
// design Q2. Substitute via constructor (`signal.NewBroker(t.TempDir(), ...)`),
// not via interface implementation.
//
// Thread-safety: all methods are safe to call from concurrent goroutines.
// The underlying filesystem operations are the synchronization primitive
// (POSIX rename is atomic; O_EXCL+O_CREATE is race-free for new files).
type Broker struct {
	controlDir   string
	consumedDir  string
	pollInterval time.Duration
}

// BrokerOption configures a Broker. Currently only one option exists; future
// additions (fsnotify, custom file modes) extend additively.
type BrokerOption func(*Broker)

// WithPollInterval sets how often Broker.Receive polls the control directory
// for new signal files. Production default is 100ms; tests use ~1ms for
// determinism. Out-of-range values (<=0) are silently ignored; production
// default applies.
func WithPollInterval(d time.Duration) BrokerOption {
	return func(b *Broker) {
		if d > 0 {
			b.pollInterval = d
		}
	}
}

// NewBroker constructs a Broker bound to controlDir. The directory is created
// LAZILY — WriteSignal / WritePause / WriteCancel each MkdirAll the controlDir
// (and consumedDir, for atomic-rename targets) on first call. NewBroker
// itself does no I/O and never returns an error.
//
// Default poll interval: 100ms (overridable via WithPollInterval).
func NewBroker(controlDir string, opts ...BrokerOption) *Broker {
	b := &Broker{
		controlDir:   controlDir,
		consumedDir:  ConsumedDir(controlDir),
		pollInterval: 100 * time.Millisecond,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// ControlDir returns the configured control directory. For tests + diagnostics.
func (b *Broker) ControlDir() string {
	return b.controlDir
}

// PollInterval returns the configured poll interval. Used by engine.Run's
// background polling goroutine (engine/controls.go) to set its ticker.
// Read-only after NewBroker — no setter; reconfigure via a fresh Broker.
func (b *Broker) PollInterval() time.Duration {
	return b.pollInterval
}

// WriteSignal writes signal-<name>-<seq>.json with payload bytes. seq is
// auto-allocated by re-reading the directory state on EACH attempt — every
// retry sees concurrent writers' just-committed seqs and bumps past them.
//
// payload may be nil (signal with no payload). Empty file is written; readers
// distinguish via len(payload) == 0.
//
// Returns the allocated seq on success.
//
// Concurrency: O_EXCL is the atomicity primitive. On ErrExist, the loop
// re-runs nextSeq() to pick up the up-to-date max, then retries — convergence
// is O(N) per writer for N concurrent invocations. maxAttempts=50 covers
// reasonable bursts (CLI operators rarely fire >50 signals concurrently);
// exhaustion returns an error (slice 3.5 design Q6 + critique C4).
func (b *Broker) WriteSignal(name string, payload []byte) (int, error) {
	if err := validateSignalName(name); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(b.controlDir, 0o755); err != nil {
		return 0, fmt.Errorf("signal: mkdir %q: %w", b.controlDir, err)
	}
	const maxAttempts = 50
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Re-read on every iteration — concurrent writers' committed seqs
		// become visible. This is the C4 fix: the original code allocated
		// startSeq ONCE and bumped a local counter, which raced under N
		// concurrent writers with N > maxAttempts.
		seq, err := b.nextSeq(name)
		if err != nil {
			return 0, err
		}
		path := filepath.Join(b.controlDir, signalFileName(name, seq))
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			if errors.Is(openErr, fs.ErrExist) {
				continue // concurrent writer claimed this seq; re-read and retry
			}
			return 0, fmt.Errorf("signal: write %q: %w", path, openErr)
		}
		if _, werr := f.Write(payload); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return 0, fmt.Errorf("signal: write payload %q: %w", path, werr)
		}
		if cerr := f.Close(); cerr != nil {
			return 0, fmt.Errorf("signal: close %q: %w", path, cerr)
		}
		return seq, nil
	}
	return 0, fmt.Errorf("signal: WriteSignal(%q): seq allocation contended out after %d attempts", name, maxAttempts)
}

// nextSeq returns max(existing-seq-for-name) + 1, considering both pending
// (controlDir/signal-<name>-N.json) and consumed (consumedDir/signal-<name>-N.json)
// files. Returns 1 if no existing signal for name.
func (b *Broker) nextSeq(name string) (int, error) {
	maxSeq := 0
	for _, dir := range []string{b.controlDir, b.consumedDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("signal: read %q: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n, seq, ok := parseSignalFileName(e.Name())
			if !ok || n != name {
				continue
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}
	}
	return maxSeq + 1, nil
}

// Delivery is the result of consuming one signal file. Name + Seq identify
// the source; Payload is the file's raw bytes (empty for no-payload signals).
type Delivery struct {
	Name    string
	Seq     int
	Payload []byte
}

// Receive blocks until a signal of name arrives, ctx cancels, or timeout
// elapses (0 = no timeout). On delivery, the file is atomic-renamed into
// consumed/ BEFORE returning (so a crash before the engine commits
// signal.received leaves the file in consumed/; on resume the engine's Fold
// re-derives state from the log, not the broker). Returns the Delivery on
// success.
//
// ctx-cancel: returns (Delivery{}, ctx.Err()).
// timeout:    returns (Delivery{}, context.DeadlineExceeded) — caller maps to
//
//	retryable_failure per spec §4.3.
func (b *Broker) Receive(ctx context.Context, name string, timeout time.Duration) (Delivery, error) {
	if err := validateSignalName(name); err != nil {
		return Delivery{}, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	// Drain first (in case a signal was written before Receive was called).
	if d, ok := b.tryConsume(name); ok {
		return d, nil
	}
	for {
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-ticker.C:
			if d, ok := b.tryConsume(name); ok {
				return d, nil
			}
		}
	}
}

// candidate is one buffered signal file matching a name — what tryConsume and
// tryConsumeMatching select from.
type candidate struct {
	seq      int
	fileName string
}

// sortedCandidates returns every buffered signal file in controlDir matching
// name, ascending by seq (nil if none, or the dir is unreadable). It is the
// shared read-only scan behind tryConsume (which takes the earliest) and
// tryConsumeMatching (which takes the first whose payload matches) — only the
// SELECTION rule and the rename tail differ, so only this scan is shared.
func (b *Broker) sortedCandidates(name string) []candidate {
	entries, err := os.ReadDir(b.controlDir)
	if err != nil {
		return nil
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, seq, ok := parseSignalFileName(e.Name())
		if !ok || n != name {
			continue
		}
		cands = append(cands, candidate{seq, e.Name()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].seq < cands[j].seq })
	return cands
}

// tryConsume scans controlDir for the EARLIEST-seq signal file matching name,
// reads its bytes, atomic-renames it into consumed/, and returns the Delivery.
// ok=false if no matching file is present.
func (b *Broker) tryConsume(name string) (Delivery, bool) {
	cands := b.sortedCandidates(name)
	if len(cands) == 0 {
		return Delivery{}, false
	}
	earliest := cands[0]
	srcPath := filepath.Join(b.controlDir, earliest.fileName)

	payload, err := os.ReadFile(srcPath)
	if err != nil {
		return Delivery{}, false
	}
	if err := os.MkdirAll(b.consumedDir, 0o755); err != nil {
		return Delivery{}, false
	}
	dstPath := filepath.Join(b.consumedDir, earliest.fileName)
	if err := os.Rename(srcPath, dstPath); err != nil {
		// Concurrent consumer claimed it; the next Receive iteration will
		// find a different match or block.
		return Delivery{}, false
	}
	return Delivery{Name: name, Seq: earliest.seq, Payload: payload}, true
}

// MatchFunc decides whether a buffered signal's payload satisfies a keyed-await
// `where:` clause. Returns (true, nil) to consume this candidate, (false, nil)
// to skip it (leave it buffered for another await), or (false, err) when the
// payload cannot be predicated (e.g. not JSON) — treated as skip-this-candidate
// by tryConsumeMatching (the engine builds the predicate; see engine/signal_step.go).
type MatchFunc func(payload []byte) (bool, error)

// ReceiveMatching is Receive with a payload predicate: it blocks until a signal
// of name whose payload satisfies match arrives, ctx cancels, or timeout elapses
// (0 = no timeout). The EARLIEST-seq matching signal is atomic-renamed into
// consumed/ before returning; non-matching (and unpredicatable) candidates are
// LEFT in place for other awaits. Same crash-safety contract as Receive (a crash
// before the engine commits signal.received leaves the file in consumed/; Fold
// re-derives state from the log, not the broker).
//
// ctx-cancel: (Delivery{}, ctx.Err()). timeout: (Delivery{}, DeadlineExceeded) —
// the engine maps to retryable_failure (spec §4.3), identical to "no signal".
func (b *Broker) ReceiveMatching(ctx context.Context, name string, timeout time.Duration, match MatchFunc) (Delivery, error) {
	if err := validateSignalName(name); err != nil {
		return Delivery{}, err
	}
	if match == nil {
		// Defense: a nil predicate degrades to plain earliest-first Receive.
		return b.Receive(ctx, name, timeout)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	if d, ok := b.tryConsumeMatching(name, match); ok {
		return d, nil
	}
	for {
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-ticker.C:
			if d, ok := b.tryConsumeMatching(name, match); ok {
				return d, nil
			}
		}
	}
}

// tryConsumeMatching scans controlDir for signals of name in ASCENDING seq order
// and consumes (atomic-renames into consumed/) the FIRST whose payload satisfies
// match. A candidate whose predicate returns (false, _) — including a predicate
// ERROR (unpredicatable payload, e.g. non-JSON) — is skipped and left buffered.
// ok=false if no candidate matches this scan. Mirrors tryConsume's read/rename
// mechanics exactly; only the selection rule differs.
func (b *Broker) tryConsumeMatching(name string, match MatchFunc) (Delivery, bool) {
	for _, c := range b.sortedCandidates(name) {
		srcPath := filepath.Join(b.controlDir, c.fileName)
		payload, rerr := os.ReadFile(srcPath)
		if rerr != nil {
			continue // disappeared / unreadable; another consumer or a transient fault
		}
		matched, merr := match(payload)
		if merr != nil || !matched {
			continue // unpredicatable or non-matching → leave buffered
		}
		if err := os.MkdirAll(b.consumedDir, 0o755); err != nil {
			return Delivery{}, false
		}
		dstPath := filepath.Join(b.consumedDir, c.fileName)
		if err := os.Rename(srcPath, dstPath); err != nil {
			continue // concurrent consumer claimed it; try the next candidate
		}
		return Delivery{Name: name, Seq: c.seq, Payload: payload}, true
	}
	return Delivery{}, false
}

// Drain returns all pending signals (any name) without blocking. Existed
// in an earlier draft for use by an at-commit-boundary journal — that draft
// has been superseded by the background pollControls goroutine architecture
// (engine/controls.go).
//
// Phase 3 minimum: Drain is NOT called by the engine — early signals are
// picked up at the matching await's Receive call (via the "drain first"
// path in Receive). Drain exists for completeness + Phase 6 obs (which may
// want to project pending signals); tested but not wired in slice 3.5.
func (b *Broker) Drain() []Delivery {
	entries, err := os.ReadDir(b.controlDir)
	if err != nil {
		return nil
	}
	var deliveries []Delivery
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, _, ok := parseSignalFileName(e.Name())
		if !ok {
			continue
		}
		if d, ok := b.tryConsume(name); ok {
			deliveries = append(deliveries, d)
		}
	}
	return deliveries
}
