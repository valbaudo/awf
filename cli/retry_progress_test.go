package cli

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
)

func TestRetryProgressFormat(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	render := newRetryProgressRenderer(&out)
	render(engine.RetryNotice{
		Path: "parallel.branch", FailedAttempt: 2, NextAttempt: 3, Attempts: 8,
		Outcome: engine.OutcomeRetryableFailure, Cause: errors.New("provider busy"), Delay: 1500 * time.Millisecond,
	})
	want := "[parallel.branch] attempt 2/8 failed: provider busy; retrying as 3/8 in 1.5s\n"
	if got := out.String(); got != want {
		t.Errorf("progress = %q, want %q", got, want)
	}
}

type concurrentProbeWriter struct {
	active  atomic.Int32
	overlap atomic.Bool
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *concurrentProbeWriter) Write(p []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	// Widen the overlap window: without a renderer-owned mutex concurrent
	// parallel/map callbacks reliably enter Write together.
	runtime.Gosched()
	time.Sleep(time.Millisecond)
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	w.active.Add(-1)
	return n, err
}

func TestRetryProgressWriterIsSerializedAcrossBranches(t *testing.T) {
	t.Parallel()
	w := &concurrentProbeWriter{}
	render := newRetryProgressRenderer(w)
	const branches = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < branches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			render(engine.RetryNotice{
				Path: "parallel.branch", FailedAttempt: 1, NextAttempt: 2, Attempts: 2,
				Outcome: engine.OutcomeRetryableFailure, Cause: errors.New("transient"),
			})
		}()
	}
	close(start)
	wg.Wait()
	if w.overlap.Load() {
		t.Error("retry progress performed concurrent writes")
	}
	w.mu.Lock()
	got := w.buf.String()
	w.mu.Unlock()
	if lines := strings.Count(got, "\n"); lines != branches {
		t.Errorf("lines = %d, want %d", lines, branches)
	}
}
