package signal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
