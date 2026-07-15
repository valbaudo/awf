package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func tempBroker(t *testing.T) *Broker {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "control")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return NewBroker(dir, WithPollInterval(time.Millisecond))
}

func TestBrokerWriteSignalSeqAllocation(t *testing.T) {
	b := tempBroker(t)
	seq, err := b.WriteSignal("name", []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("WriteSignal #1: %v", err)
	}
	if seq != 1 {
		t.Errorf("first WriteSignal: seq = %d, want 1", seq)
	}
	seq, err = b.WriteSignal("name", []byte(`{"v":2}`))
	if err != nil {
		t.Fatalf("WriteSignal #2: %v", err)
	}
	if seq != 2 {
		t.Errorf("second WriteSignal: seq = %d, want 2", seq)
	}
	// Different name: independent counter.
	seq, err = b.WriteSignal("other", nil)
	if err != nil {
		t.Fatalf("WriteSignal #3: %v", err)
	}
	if seq != 1 {
		t.Errorf("different-name first: seq = %d, want 1", seq)
	}
}

func TestBrokerWriteSignalRejectsInvalidName(t *testing.T) {
	// M16: signal name must match signalNamePattern (no whitespace, no
	// path separators, etc.). Both WriteSignal and Receive enforce.
	b := tempBroker(t)
	bad := []string{
		"human review", // space
		"../escape",    // path traversal
		"name\x00",     // nullbyte
		"0day",         // leading digit
		"",             // empty
		"name\nwith\nnewlines",
	}
	for _, name := range bad {
		_, err := b.WriteSignal(name, []byte("p"))
		if err == nil {
			t.Errorf("WriteSignal(%q) should have errored", name)
		}
	}
	// Sanity: legal names accepted.
	for _, name := range []string{"human_review", "tick-tock", "_internal", "x"} {
		if _, err := b.WriteSignal(name, []byte("p")); err != nil {
			t.Errorf("WriteSignal(%q): %v", name, err)
		}
	}
}

func TestBrokerReceiveRejectsInvalidName(t *testing.T) {
	b := tempBroker(t)
	_, err := b.Receive(context.Background(), "../escape", time.Millisecond)
	if err == nil {
		t.Error("Receive with invalid name should have errored")
	}
}

func TestBrokerWriteSignalConcurrent(t *testing.T) {
	b := tempBroker(t)
	const N = 16
	var wg sync.WaitGroup
	seqs := make([]int, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			seqs[i], errs[i] = b.WriteSignal("contended", []byte("x"))
		}()
	}
	wg.Wait()
	// All N must succeed with distinct seqs 1..N.
	seen := map[int]bool{}
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("WriteSignal[%d]: err = %v", i, errs[i])
			continue
		}
		if seen[seqs[i]] {
			t.Errorf("duplicate seq %d", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for n := 1; n <= N; n++ {
		if !seen[n] {
			t.Errorf("missing seq %d", n)
		}
	}
}

func TestBrokerReceiveDeliversInSeqOrder(t *testing.T) {
	b := tempBroker(t)
	if _, err := b.WriteSignal("name", []byte("first")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := b.WriteSignal("name", []byte("second")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	ctx := context.Background()
	d1, err := b.Receive(ctx, "name", time.Second)
	if err != nil {
		t.Fatalf("Receive 1: %v", err)
	}
	if string(d1.Payload) != "first" || d1.Seq != 1 {
		t.Errorf("Receive 1: %+v", d1)
	}
	d2, err := b.Receive(ctx, "name", time.Second)
	if err != nil {
		t.Fatalf("Receive 2: %v", err)
	}
	if string(d2.Payload) != "second" || d2.Seq != 2 {
		t.Errorf("Receive 2: %+v", d2)
	}
}

func TestBrokerReceiveCtxCancel(t *testing.T) {
	b := tempBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := b.Receive(ctx, "never", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Receive: err = %v, want context.Canceled", err)
	}
}

func TestBrokerReceiveTimeout(t *testing.T) {
	b := tempBroker(t)
	_, err := b.Receive(context.Background(), "never", 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Receive: err = %v, want DeadlineExceeded", err)
	}
}

func TestBrokerReceiveReadDirErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	b.ops.readDir = func(string) ([]os.DirEntry, error) { return nil, fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Receive(ctx, "ready", 0)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Receive error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveReadFileErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Receive(ctx, "ready", 0)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Receive error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveMatchingReadFileErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.ReceiveMatching(ctx, "ready", 0, func([]byte) (bool, error) { return true, nil })
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReceiveMatching error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveMkdirAllErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.mkdirAll = func(string, fs.FileMode) error { return fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Receive(ctx, "ready", 0)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Receive error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveMatchingMkdirAllErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.mkdirAll = func(string, fs.FileMode) error { return fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.ReceiveMatching(ctx, "ready", 0, func([]byte) (bool, error) { return true, nil })
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReceiveMatching error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveRenameErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.rename = func(string, string) error { return fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Receive(ctx, "ready", 0)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Receive error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveMatchingRenameErrorReturnsImmediately(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.rename = func(string, string) error { return fs.ErrPermission }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.ReceiveMatching(ctx, "ready", 0, func([]byte) (bool, error) { return true, nil })
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReceiveMatching error = %v, want fs.ErrPermission before context cancellation", err)
	}
}

func TestBrokerReceiveRenameNotExistWithSourcePresentReturnsError(t *testing.T) {
	b := tempBroker(t)
	if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ops.rename = func(string, string) error { return fs.ErrNotExist }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Receive(ctx, "ready", 0)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Receive error = %v, want rename fs.ErrNotExist while source remains present", err)
	}
}

func TestBrokerReceiveRenameNotExistIgnoredAfterSourceDisappears(t *testing.T) {
	for _, matching := range []bool{false, true} {
		matching := matching
		t.Run(fmt.Sprintf("matching=%t", matching), func(t *testing.T) {
			b := tempBroker(t)
			if err := os.WriteFile(filepath.Join(b.controlDir, signalFileName("ready", 1)), []byte("payload"), 0o600); err != nil {
				t.Fatal(err)
			}
			b.ops.rename = func(src, _ string) error {
				if err := os.Remove(src); err != nil {
					t.Fatal(err)
				}
				return fs.ErrNotExist
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			var err error
			if matching {
				_, err = b.ReceiveMatching(ctx, "ready", 0, func([]byte) (bool, error) { return true, nil })
			} else {
				_, err = b.Receive(ctx, "ready", 0)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("receive error = %v, want context.Canceled after concurrent source disappearance", err)
			}
		})
	}
}

func TestBrokerReceiveDrainsEarlySignal(t *testing.T) {
	// Signal written BEFORE Receive is called — first poll (or drain-first)
	// picks it up without blocking.
	b := tempBroker(t)
	if _, err := b.WriteSignal("early", []byte("hi")); err != nil {
		t.Fatalf("WriteSignal: %v", err)
	}
	d, err := b.Receive(context.Background(), "early", time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(d.Payload) != "hi" {
		t.Errorf("Receive: payload = %q, want hi", d.Payload)
	}
	// File moved into consumed/.
	pendingFiles, _ := os.ReadDir(b.controlDir)
	for _, e := range pendingFiles {
		if e.IsDir() {
			continue
		}
		t.Errorf("pending file not consumed: %q", e.Name())
	}
	consumedFiles, _ := os.ReadDir(b.consumedDir)
	if len(consumedFiles) != 1 {
		t.Errorf("consumed/ has %d files, want 1", len(consumedFiles))
	}
}

func TestBrokerPauseCancelCheck(t *testing.T) {
	b := tempBroker(t)
	// Initially neither file exists.
	p, c, err := b.CheckPauseCancel()
	if err != nil {
		t.Fatalf("CheckPauseCancel: %v", err)
	}
	if p != nil || c != nil {
		t.Errorf("initial check: pause=%+v cancel=%+v, want both nil", p, c)
	}
	// Write pause.
	if err := b.WritePause(PauseRequest{NodePath: "step.x", Reason: "test"}); err != nil {
		t.Fatalf("WritePause: %v", err)
	}
	p, c, _ = b.CheckPauseCancel()
	if p == nil || p.NodePath != "step.x" || p.Reason != "test" {
		t.Errorf("after WritePause: pause=%+v", p)
	}
	if c != nil {
		t.Errorf("cancel = %+v, want nil", c)
	}
	// Write cancel.
	if err := b.WriteCancel(CancelRequest{Reason: "test"}); err != nil {
		t.Fatalf("WriteCancel: %v", err)
	}
	p, c, _ = b.CheckPauseCancel()
	if p == nil {
		t.Errorf("pause cleared by cancel; want still set")
	}
	if c == nil || c.Reason != "test" {
		t.Errorf("after WriteCancel: cancel=%+v", c)
	}
}

func TestBrokerClearPauseCancel(t *testing.T) {
	b := tempBroker(t)
	_ = b.WritePause(PauseRequest{})
	_ = b.WriteCancel(CancelRequest{})
	if err := b.ClearPauseCancel(); err != nil {
		t.Fatalf("ClearPauseCancel: %v", err)
	}
	p, c, _ := b.CheckPauseCancel()
	if p != nil || c != nil {
		t.Errorf("post-clear: pause=%+v cancel=%+v, want both nil", p, c)
	}
	// Idempotent.
	if err := b.ClearPauseCancel(); err != nil {
		t.Errorf("idempotent clear: err = %v", err)
	}
}

func TestBrokerCheckPauseCancelCapsReadSize(t *testing.T) {
	// L10 fix: defense against adversarial oversized control files. An
	// attacker who can write to the control directory could fill cancel.json
	// with gigabytes; the broker reads at most maxControlFileBytes (64KiB).
	//
	// M18 refinement: assert the cap is APPLIED via wall-clock timing, not
	// just "doesn't OOM." Reading a 100MB file uncapped takes ~hundreds of
	// ms (disk I/O); the cap caps it at 64KB → microseconds. A 100ms ceiling
	// is well above the capped read time + well below an uncapped read time.
	b := tempBroker(t)
	// Write 100MB (1000x the cap) — uncapped read would take ~hundreds of ms.
	huge := make([]byte, 100*1024*1024)
	cancelPath := filepath.Join(b.controlDir, "cancel.json")
	if err := os.WriteFile(cancelPath, huge, 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, cancelReq, err := b.CheckPauseCancel()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CheckPauseCancel on oversized file: %v", err)
	}
	if cancelReq == nil {
		t.Errorf("cancelReq = nil; want non-nil (file exists; truncated read should still detect presence)")
	}
	// 100ms is generous on slow CI; uncapped 100MB read would take much longer.
	// If this fails, the cap regressed — investigate before bumping the limit.
	if elapsed > 100*time.Millisecond {
		t.Errorf("CheckPauseCancel took %s on 100MB file; cap (%d bytes) regressed (uncapped read would be ~1000x longer)", elapsed, maxControlFileBytes)
	}
}

func TestBrokerDrainReturnsAll(t *testing.T) {
	b := tempBroker(t)
	if _, err := b.WriteSignal("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteSignal("b", []byte("2")); err != nil {
		t.Fatal(err)
	}
	got := b.Drain()
	if len(got) != 2 {
		t.Errorf("Drain returned %d, want 2", len(got))
	}
}

func TestBrokerReceiveMatchingEarliest(t *testing.T) {
	b := tempBroker(t)
	// Two buffered signals; only seq 2 matches the predicate.
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"a"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"b"}`)); err != nil {
		t.Fatal(err)
	}
	match := func(p []byte) (bool, error) {
		return strings.Contains(string(p), `"candidate_id":"b"`), nil
	}
	d, err := b.ReceiveMatching(context.Background(), "oob", 0, match)
	if err != nil {
		t.Fatalf("ReceiveMatching: %v", err)
	}
	if d.Seq != 2 || !strings.Contains(string(d.Payload), `"b"`) {
		t.Errorf("consumed wrong signal: seq=%d payload=%s", d.Seq, d.Payload)
	}
	// The non-matching seq 1 must still be buffered (consumable plainly).
	d1, err := b.Receive(context.Background(), "oob", 0)
	if err != nil {
		t.Fatalf("Receive remaining: %v", err)
	}
	if d1.Seq != 1 {
		t.Errorf("remaining signal seq=%d, want 1 (non-match must stay buffered)", d1.Seq)
	}
}

func TestBrokerReceiveMatchingEarliestWins(t *testing.T) {
	b := tempBroker(t)
	// Both match; the EARLIEST seq must win.
	if _, err := b.WriteSignal("oob", []byte(`{"k":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteSignal("oob", []byte(`{"k":"x"}`)); err != nil {
		t.Fatal(err)
	}
	match := func(p []byte) (bool, error) { return true, nil }
	d, err := b.ReceiveMatching(context.Background(), "oob", 0, match)
	if err != nil {
		t.Fatal(err)
	}
	if d.Seq != 1 {
		t.Errorf("seq=%d, want 1 (earliest match)", d.Seq)
	}
}

func TestBrokerReceiveMatchingTimeout(t *testing.T) {
	b := tempBroker(t)
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"a"}`)); err != nil {
		t.Fatal(err)
	}
	// Predicate never matches → blocks → timeout (the non-match stays buffered).
	match := func(p []byte) (bool, error) { return false, nil }
	_, err := b.ReceiveMatching(context.Background(), "oob", 5*time.Millisecond, match)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestBrokerReceiveMatchingSkipsMalformedPayload(t *testing.T) {
	b := tempBroker(t)
	if _, err := b.WriteSignal("oob", []byte("not json")); err != nil { // seq 1: predicate errors
		t.Fatal(err)
	}
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"b"}`)); err != nil { // seq 2: matches
		t.Fatal(err)
	}
	match := func(p []byte) (bool, error) {
		if !json.Valid(p) {
			return false, fmt.Errorf("payload not JSON")
		}
		return strings.Contains(string(p), `"b"`), nil
	}
	d, err := b.ReceiveMatching(context.Background(), "oob", 0, match)
	if err != nil {
		t.Fatalf("ReceiveMatching: %v", err)
	}
	if d.Seq != 2 {
		t.Errorf("seq=%d, want 2 (malformed seq 1 must be skipped, not consumed)", d.Seq)
	}
}
