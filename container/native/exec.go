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
// Streaming pattern (Appendix C in the design spec): StdoutPipe +
// StderrPipe + sync.WaitGroup. io.Copy errors are intentionally
// discarded (pipe-close on process exit is normal, not propagable).
// c.Wait() error discarded; ProcessState.ExitCode() is the meaningful
// signal (matches docker's ContainerExecInspect pattern).
//
// ctx-cancel: Go 1.20+ exec.CommandContext sets Cmd.Cancel =
// cmd.Process.Kill by default. ctx.Done() → SIGKILL → pipes close →
// reader goroutines exit → wg.Wait returns. We check ctx.Err() AFTER
// Wait so it dominates *exec.ExitError.
//
// Phase 2 contract: returned channel is buffered, pre-filled with
// every IOChunk emitted, and closed before Exec returns. On non-nil
// error, the channel is nil (callers MUST check err before ranging).
func (b *Backend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (container.ExecResult, <-chan container.IOChunk, error) {
	if err := ctx.Err(); err != nil {
		return container.ExecResult{}, nil, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return container.ExecResult{}, nil, errors.New("container/native: Exec: unknown handle (not Created or already Destroyed)")
	}

	c := exec.CommandContext(ctx, "sh", "-c", cmd.Run)
	c.Dir = r.workdir
	c.Env = append(c.Environ(), envMapToSlice(cmd.Env)...)

	stdoutPipe, err := c.StdoutPipe()
	if err != nil {
		return container.ExecResult{}, nil, err
	}
	stderrPipe, err := c.StderrPipe()
	if err != nil {
		return container.ExecResult{}, nil, err
	}
	if err := c.Start(); err != nil {
		return container.ExecResult{}, nil, err
	}

	var chunksMu sync.Mutex
	var chunks []container.IOChunk
	stdoutBuf := newChunkBuffer("stdout", &chunks, &chunksMu)
	stderrBuf := newChunkBuffer("stderr", &chunks, &chunksMu)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(stdoutBuf, stdoutPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(stderrBuf, stderrPipe) }()
	wg.Wait()
	_ = c.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return container.ExecResult{}, nil, ctxErr
	}

	exitCode := c.ProcessState.ExitCode()
	out := make(chan container.IOChunk, len(chunks))
	for _, ch := range chunks {
		out <- ch
	}
	close(out)

	return container.ExecResult{
		ExitCode:  exitCode,
		Stdout:    stdoutBuf.collected,
		AWFOutput: nil, // dispatcher reads AWF_OUTPUT tempfile via CaptureFiles (Phase 4 design §B)
	}, out, nil
}

// chunkBuffer is an io.Writer wrapping IOChunk emission + (for stdout)
// stdout accumulation. Mutex-protected because two reader goroutines
// share the chunks slice. Defensive byte-copy in Write because
// io.Copy reuses its read buffer.
type chunkBuffer struct {
	stream    string
	chunks    *[]container.IOChunk
	mu        *sync.Mutex
	collected []byte
}

func newChunkBuffer(stream string, chunks *[]container.IOChunk, mu *sync.Mutex) *chunkBuffer {
	return &chunkBuffer{stream: stream, chunks: chunks, mu: mu}
}

func (cb *chunkBuffer) Write(p []byte) (int, error) {
	dup := make([]byte, len(p))
	copy(dup, p)
	cb.mu.Lock()
	*cb.chunks = append(*cb.chunks, container.IOChunk{Stream: cb.stream, Data: dup})
	cb.mu.Unlock()
	if cb.stream == "stdout" {
		cb.collected = append(cb.collected, dup...)
	}
	return len(p), nil
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
