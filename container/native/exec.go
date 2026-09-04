package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/valbaudo/awf/container"
)

// execWaitDelay bounds how long, after the process exits or ctx is cancelled, Go
// waits for the command's I/O pipes to drain before forcibly closing them. It is
// the cross-platform backstop to the process-group reap (hardenProcessCleanup):
// even if a workload double-forks a descendant out of the group and that
// descendant keeps a pipe write-end open, the reader goroutines are guaranteed to
// unblock within this bound instead of deadlocking the run forever. Generous
// enough never to fire on a healthy run (the process's own exit closes the pipes
// first).
const execWaitDelay = 10 * time.Second

// Exec runs cmd.Run via `sh -c` with cmd.Dir = handle workdir. Host
// env is inherited and merged with cmd.Env (cmd.Env wins on conflict).
// Stdout is accumulated into ExecResult.Stdout; stderr surfaces only
// via the IOChunk channel (per AWF spec §4.1 — implicit outputs are
// exit_code and stdout only).
//
// Streaming contract (slice 5.3): returns (chunks, result, error).
// Per-pipe reader goroutines emit IOChunks live as bytes arrive; a
// waiter goroutine c.Waits while both readers drain, then waits for the
// readers, closes chunks, computes the ExitCode and emits ONE ExecResult on
// the 1-buffered result channel. Calling Wait first is required for WaitDelay
// to close pipe descriptors inherited by descendants after the child exits.
//
// ctx-cancel: the default exec.CommandContext cancel SIGKILLs only the
// DIRECT child, but the workload runs as a GRANDCHILD (under `sh -c`, and a
// sandbox trampoline in production), so a bare kill leaves the grandchild
// alive holding the pipe write-ends open — the reader goroutines never see
// EOF and the run deadlocks (customer-reported; docker solved the same class
// with killExecTree, #18). hardenProcessCleanup (below, before Start)
// replaces the default with a process-GROUP kill (Setpgid + SIGKILL -pgid),
// reaping the grandchild and closing the pipes; Cmd.WaitDelay is the backstop
// for a straggler that escapes the group. On cancel the ctx.Err surfaces via
// ExecResult.Err (not the err return) so the result channel carries it
// without short-circuiting the chunk drain. c.Wait's error is discarded;
// ProcessState.ExitCode() is the meaningful signal (matches docker's
// ContainerExecInspect pattern and pre-5.3 native behavior).
//
// Phase 2 + slice 5.3 contract: on non-nil err return, BOTH channels
// are nil (callers MUST check err before receiving on either).
func (b *Backend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return nil, nil, errors.New("container/native: Exec: unknown handle (not Created or already Destroyed)")
	}

	// Resolve the per-dispatch launcher. When a sandbox is configured and it
	// is a factory (bwrap or trampoline), buildForRun produces a launcher with
	// the run-command baked in. When no sandbox is configured (nil) or the
	// launcher is the no-op, argv is nil and we fall through to the plain sh
	// invocation below — identical behaviour to pre-sandbox code.
	var c *exec.Cmd
	if launcher := b.resolvedSandbox(); launcher != nil {
		var argv []string
		if factory, ok := launcher.(sandboxLauncherFactory); ok {
			// Per-dispatch factory: bwrap and landlock-trampoline launchers.
			// runHome is the process's HOME so credDirs resolves user config dirs.
			runHome := os.Getenv("HOME")
			perRunLauncher := factory.buildForRun(cmd.Run)
			// Grants are scoped to the KIND of exec: only an agent adapter running
			// its own CLI gets the tool prefixes (so it stays executable under the
			// tmpfs'd HOME instead of exiting 126) and writable credential dirs
			// (so a token refresh persists). A code step gets neither.
			rwDirs, roDirs := sandboxDirsFor(cmd, runHome)
			if err := ensureWritableDirs(rwDirs); err != nil {
				return nil, nil, err
			}
			argv = perRunLauncher.prepend(r.workdir, rwDirs, roDirs)
		} else {
			// Non-factory launcher (e.g. noOpLauncher) returns nil from prepend.
			argv = launcher.prepend(r.workdir, nil, nil)
		}
		if argv != nil {
			c = exec.CommandContext(ctx, argv[0], argv[1:]...)
		}
	}
	if c == nil {
		// No sandbox or no-op launcher: run sh directly (pre-sandbox behaviour).
		c = exec.CommandContext(ctx, "sh", "-c", cmd.Run)
	}
	c.Dir = r.workdir
	c.Env = append(c.Environ(), envMapToSlice(cmd.Env)...)

	// Reap the whole process tree on ctx-cancel (timeout), not just the direct
	// child: the workload runs under `sh -c` / a sandbox trampoline as a
	// grandchild, which would otherwise survive SIGKILL and hold the stdout/stderr
	// pipe write-ends open, deadlocking the reader goroutines below (the bug the
	// docker backend fixed via killExecTree, #18). WaitDelay is the cross-platform
	// backstop for a straggler that escapes the process group.
	hardenProcessCleanup(c)
	c.WaitDelay = execWaitDelay

	stdoutPipe, err := c.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderrPipe, err := c.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := c.Start(); err != nil {
		return nil, nil, err
	}

	// Modest buffer keeps the writer goroutines from blocking on a slow
	// reader for a few chunks; deeper backpressure naturally throttles the
	// process if the reader is genuinely slow. 64 matches stdcopy's default
	// buffer count for docker.
	chunks := make(chan container.IOChunk, 64)
	result := make(chan container.ExecResult, 1)

	var stdoutMu sync.Mutex
	var stdoutAccum []byte
	var wg sync.WaitGroup
	wg.Add(2)
	emit := func(stream string, pipe io.Reader, accum *[]byte) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := pipe.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				chunks <- container.IOChunk{Stream: stream, Data: data}
				if accum != nil {
					stdoutMu.Lock()
					*accum = append(*accum, data...)
					stdoutMu.Unlock()
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	go emit("stdout", stdoutPipe, &stdoutAccum)
	go emit("stderr", stderrPipe, nil)

	go func() {
		_ = c.Wait()
		wg.Wait()
		close(chunks)
		exitCode := c.ProcessState.ExitCode()
		stdoutMu.Lock()
		out := append([]byte(nil), stdoutAccum...)
		stdoutMu.Unlock()
		// Surface ctx-cancel via ExecResult.Err (pre-slice-5.3 used err return).
		var resErr error
		if ctxErr := ctx.Err(); ctxErr != nil {
			resErr = ctxErr
		}
		result <- container.ExecResult{
			ExitCode:  exitCode,
			Stdout:    out,
			AWFOutput: nil, // dispatcher reads AWF_OUTPUT tempfile via CaptureFiles (Phase 4 design §B)
			Err:       resErr,
		}
		close(result)
	}()

	return chunks, result, nil
}

// ensureWritableDirs pre-creates the sandbox's read-write grant dirs. The
// landlock launcher binds grants with IgnoreIfMissing — correct for optional
// host dirs, but a credential dir that does not exist YET (fresh runner, no
// ~/.codex) gets its grant silently skipped, and the agent CLI's own mkdir
// then runs INSIDE the sandbox and dies EACCES (run 9486dda3, 2026-08-16).
// Create before launch so the grant has something to bind to.
func ensureWritableDirs(dirs []string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("container/native: create writable dir %q: %w", d, err)
		}
	}
	return nil
}

// resolvedSandbox returns the configured sandboxLauncher or nil.
// nil means no sandbox was requested (WithSandbox(false) or not supplied);
// non-nil means sandbox was requested (may be noOpLauncher or a real launcher).
// Re-added here after the golangci unused-removal in Task 2.
func (b *Backend) resolvedSandbox() sandboxLauncher {
	return b.sandbox
}

// envMapToSlice converts cmd.Env map into the "KEY=value" []string
// format exec.Cmd.Env expects. Sorted with sort.Strings for
// deterministic golden output (matches docker's
// container/docker/exec.go:196-210 pattern). Returns nil for empty
// or nil input.
func envMapToSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
