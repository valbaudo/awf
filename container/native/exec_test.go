package native_test

import (
	"context"
	"testing"
	"time"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestNative_Exec_ChunksArriveDuringExec asserts the slice-5.3 streaming
// contract: IOChunks emit on the chunks channel as bytes arrive on the
// pipes, NOT in a single burst after the process exits. The script emits
// one line every 100ms for 5 iterations; we verify chunks arrive
// progressively by measuring the wall-clock span between first and last
// stdout chunk.
//
// The 200ms threshold is below the script's expected ~400ms span (4
// sleeps of 100ms between 5 lines) — enough slack for slow CI without
// admitting a buffer-then-burst impl.
func TestNative_Exec_ChunksArriveDuringExec(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })

	// Script that emits one line every 100ms for 5 iterations.
	cmd := container.Cmd{Run: "for i in 1 2 3 4 5; do echo line-$i; sleep 0.1; done"}
	start := time.Now()
	chunks, result, err := b.Exec(context.Background(), h, cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var arrivals []time.Duration
	for c := range chunks {
		if c.Stream == "stdout" {
			arrivals = append(arrivals, time.Since(start))
		}
	}
	if len(arrivals) < 2 {
		t.Fatalf("got %d stdout chunks; want >=2 (proves chunks emit before process exit)", len(arrivals))
	}
	first := arrivals[0]
	last := arrivals[len(arrivals)-1]
	if last-first < 200*time.Millisecond {
		t.Errorf("first->last chunk span = %v; want >=200ms (proves chunks arrive progressively, not at-end)", last-first)
	}
	r := <-result
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", r.ExitCode)
	}
}

func TestNative_Exec_ClosesPipeInheritedByExitedProcessDescendant(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })

	start := time.Now()
	chunks, result, err := b.Exec(context.Background(), h, container.Cmd{Run: "sleep 30 & printf parent-exited"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range chunks {
	}
	res := <-result
	if elapsed := time.Since(start); elapsed >= 15*time.Second {
		t.Fatalf("Exec waited %v for a descendant-held pipe; WaitDelay never activated", elapsed)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "parent-exited" {
		t.Fatalf("result = exit %d stdout %q", res.ExitCode, res.Stdout)
	}
}
