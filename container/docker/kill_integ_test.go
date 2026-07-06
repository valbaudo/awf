//go:build integ

package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	cont "github.com/valbaudo/awf/container"
)

// execStdout runs a check command in the container via Backend.Exec and returns
// its accumulated stdout. Used to observe container process state.
func execStdout(t *testing.T, b *Backend, h cont.Handle, run string) string {
	t.Helper()
	chunks, result, err := b.Exec(context.Background(), h, cont.Cmd{Run: run})
	if err != nil {
		t.Fatalf("check Exec(%q): %v", run, err)
	}
	var sb strings.Builder
	for c := range chunks {
		if c.Stream == "stdout" {
			sb.Write(c.Data)
		}
	}
	<-result
	return sb.String()
}

// TestDocker_Exec_KillsProcessTreeOnCancel verifies the #18 fix: when a step's
// ctx is cancelled (timeout), the docker backend reaps the step's in-container
// process tree instead of leaving it running until container teardown. The step
// backgrounds a child `sleep` under the wrapping shell (PPID-parented to the
// tree root) and stalls; after cancel, the child must be gone.
func TestDocker_Exec_KillsProcessTreeOnCancel(t *testing.T) {
	cli, b := newTestBackend(t, "kill-tree")
	h := newAlpineContainer(t, cli, b)

	// Sanity: nothing matching yet.
	if out := strings.TrimSpace(execStdout(t, b, h, "pgrep -f '[s]leep 12345' || true")); out != "" {
		t.Fatalf("precondition: sleep 12345 already running: %q", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Leader sh backgrounds a child sleep then waits → stalls with no output.
	chunks, result, err := b.Exec(ctx, h, cont.Cmd{Run: "sleep 12345 & wait"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range chunks {
	}
	res := <-result
	if res.Err == nil {
		t.Fatalf("expected ExecResult.Err after ctx cancel, got nil (exit=%d)", res.ExitCode)
	}

	// The reaper is detached + bounded; poll until the child is gone.
	deadline := time.Now().Add(8 * time.Second)
	for {
		out := strings.TrimSpace(execStdout(t, b, h, "pgrep -f '[s]leep 12345' || true"))
		if out == "" {
			return // reaped — success
		}
		if time.Now().After(deadline) {
			t.Fatalf("child 'sleep 12345' STILL running ~8s after cancel; reaper failed (pgrep=%q)", out)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestDocker_Exec_NormalCompletionUnaffected guards that the pidfile wrapper does
// not disturb a normal (non-cancelled) step: stdout and exit code pass through.
func TestDocker_Exec_NormalCompletionUnaffected(t *testing.T) {
	cli, b := newTestBackend(t, "kill-normal")
	h := newAlpineContainer(t, cli, b)

	chunks, result, err := b.Exec(context.Background(), h, cont.Cmd{Run: "echo hello-tree; exit 7"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var sb strings.Builder
	for c := range chunks {
		if c.Stream == "stdout" {
			sb.Write(c.Data)
		}
	}
	res := <-result
	if res.Err != nil {
		t.Fatalf("unexpected Err on normal completion: %v", res.Err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7 (wrapper must preserve exit status)", res.ExitCode)
	}
	if !strings.Contains(sb.String(), "hello-tree") {
		t.Fatalf("stdout = %q, want it to contain hello-tree (wrapper must not swallow stdout)", sb.String())
	}
}
