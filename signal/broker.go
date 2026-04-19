package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrPaused is the sentinel engine.Run returns alongside Outcome("") when
// control-file polling detects pause.json. The CLI's runAndFinish maps this
// to a clean exit (rc=0) WITHOUT writing run.finished — the run is non-
// terminal and resumable.
var ErrPaused = errors.New("signal: run paused (non-terminal)")

// ErrCancelled is the sentinel engine.Run returns alongside Outcome("") when
// control-file polling detects cancel.json. The engine has ALREADY appended
// the terminal run.cancelled event; the CLI exits cleanly. `awf resume`
// refuses any log with a run.cancelled event.
var ErrCancelled = errors.New("signal: run cancelled (terminal)")

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

// tryConsume scans controlDir for the EARLIEST-seq signal file matching name,
// reads its bytes, atomic-renames it into consumed/, and returns the Delivery.
// ok=false if no matching file is present.
func (b *Broker) tryConsume(name string) (Delivery, bool) {
	entries, err := os.ReadDir(b.controlDir)
	if err != nil {
		return Delivery{}, false
	}
	// Find all matching entries; pick earliest seq.
	type match struct {
		seq      int
		fileName string
	}
	var matches []match
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, seq, ok := parseSignalFileName(e.Name())
		if !ok || n != name {
			continue
		}
		matches = append(matches, match{seq, e.Name()})
	}
	if len(matches) == 0 {
		return Delivery{}, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].seq < matches[j].seq })
	earliest := matches[0]
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

// PauseRequest is the parsed body of pause.json.
type PauseRequest struct {
	NodePath string `json:"node_path,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// CancelRequest is the parsed body of cancel.json.
type CancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// WritePause writes pause.json. Idempotent — overwrites any existing file.
func (b *Broker) WritePause(req PauseRequest) error {
	if err := os.MkdirAll(b.controlDir, 0o755); err != nil {
		return fmt.Errorf("signal: mkdir %q: %w", b.controlDir, err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("signal: marshal pause: %w", err)
	}
	path := filepath.Join(b.controlDir, pauseFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("signal: write %q: %w", path, err)
	}
	return nil
}

// WriteCancel writes cancel.json. Idempotent.
func (b *Broker) WriteCancel(req CancelRequest) error {
	if err := os.MkdirAll(b.controlDir, 0o755); err != nil {
		return fmt.Errorf("signal: mkdir %q: %w", b.controlDir, err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("signal: marshal cancel: %w", err)
	}
	path := filepath.Join(b.controlDir, cancelFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("signal: write %q: %w", path, err)
	}
	return nil
}

// maxControlFileBytes caps how much we read from pause.json / cancel.json.
// L10 fix: defense against an adversarial / misconfigured writer that
// redirects a huge stream into the control file (e.g. `head -c 1G /dev/zero
// > control/cancel.json`). AWF is an offensive security tool; adversarial
// inputs are part of the threat model. 64KiB is more than enough for a
// reasonable JSON reason string.
const maxControlFileBytes = 64 * 1024

// readControlFile reads up to maxControlFileBytes from path. Returns nil
// (no err) if the file doesn't exist. Errors only on I/O failures or
// genuinely-unexpected conditions.
func readControlFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxControlFileBytes))
}

// CheckPauseCancel reports whether pause.json and cancel.json exist in
// controlDir. Either bool may be true; both may be true (cancel-wins resolution
// is the caller's responsibility — engine/controls.go does that).
//
// Returns (pauseReq, cancelReq, err). pauseReq/cancelReq are non-nil iff their
// respective files exist. err is non-nil only on read errors (file exists but
// not readable; malformed JSON is silently treated as an empty body). Reads
// are capped at maxControlFileBytes (L10 fix).
func (b *Broker) CheckPauseCancel() (*PauseRequest, *CancelRequest, error) {
	pausePath := filepath.Join(b.controlDir, pauseFileName)
	cancelPath := filepath.Join(b.controlDir, cancelFileName)

	var pauseReq *PauseRequest
	if data, err := readControlFile(pausePath); err != nil {
		return nil, nil, fmt.Errorf("signal: read pause %q: %w", pausePath, err)
	} else if data != nil {
		var req PauseRequest
		_ = json.Unmarshal(data, &req) // empty/malformed treated as empty body
		pauseReq = &req
	}

	var cancelReq *CancelRequest
	if data, err := readControlFile(cancelPath); err != nil {
		return nil, nil, fmt.Errorf("signal: read cancel %q: %w", cancelPath, err)
	} else if data != nil {
		var req CancelRequest
		_ = json.Unmarshal(data, &req)
		cancelReq = &req
	}
	return pauseReq, cancelReq, nil
}

// ClearPauseCancel removes pause.json and cancel.json. Idempotent (missing
// files are not errors). Called by cli/resume.go before re-entering the
// engine — pause is non-terminal but stale pause.json would re-pause on the
// next commit; resume must clear it to make forward progress.
func (b *Broker) ClearPauseCancel() error {
	for _, name := range []string{pauseFileName, cancelFileName} {
		path := filepath.Join(b.controlDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("signal: clear %q: %w", path, err)
		}
	}
	return nil
}
