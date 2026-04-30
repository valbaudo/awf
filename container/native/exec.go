package native

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sort"
	"sync"

	"github.com/valbaudo/awf/container"
)

// Exec runs cmd.Run via `sh -c` with cmd.Dir = handle workdir. Host
// env is inherited and merged with cmd.Env (cmd.Env wins on conflict).
// Stdout is accumulated into ExecResult.Stdout; stderr surfaces only
// via the IOChunk channel (per AWF spec §4.1 — implicit outputs are
// exit_code and stdout only).
//
// Streaming contract (slice 5.3): returns (chunks, result, error).
// Per-pipe reader goroutines emit IOChunks live as bytes arrive; a
// waiter goroutine wg.Waits both readers, then c.Wait()s the process,
// closes chunks, computes the ExitCode and emits ONE ExecResult on the
// 1-buffered result channel. chunks closes BEFORE result emits — every
// chunk is materialized before the result is observable.
//
// ctx-cancel: Go 1.20+ exec.CommandContext sets Cmd.Cancel =
// cmd.Process.Kill by default. ctx.Done() -> SIGKILL -> pipes close ->
// reader goroutines exit -> wg.Wait returns. Pre-slice-5.3 the ctx.Err
// came back via Backend.Exec's err return; the streaming refactor
// surfaces it via ExecResult.Err instead so the result channel can
// carry it without short-circuiting the chunk drain. c.Wait's error is
// discarded; ProcessState.ExitCode() is the meaningful signal (matches
// docker's ContainerExecInspect pattern and pre-5.3 native behavior).
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

	c := exec.CommandContext(ctx, "sh", "-c", cmd.Run)
	c.Dir = r.workdir
	c.Env = append(c.Environ(), envMapToSlice(cmd.Env)...)

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
		wg.Wait()
		_ = c.Wait()
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
