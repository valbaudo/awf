package docker

import (
	"context"
	"fmt"
	"sort"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/valbaudo/awf/container"
)

// Exec runs cmd inside the container the handle references. Returns the full
// ExecResult (ExitCode, Stdout, AWFOutput=nil), a CLOSED channel pre-filled
// with the IOChunks the command produced, and a nil err on the happy path.
//
// ExecResult.AWFOutput is left nil — the dispatcher reads the AWF_OUTPUT
// tempfile via CaptureFiles per the Phase 4 design §B contract (slice 4.2
// Design Q1). A future Backend may populate it; the dispatcher handles both.
//
// Contract preserved from Phase 2 (channel closed before Exec returns) per
// slice 4.2 Design Q3. True-live tap (channel open while caller consumes
// live) requires interpreter changes that are out of scope for this slice;
// it is tracked as a Phase 4 follow-up.
//
// Shell: cmd.Run is passed as a single string to `sh -c` (POSIX baseline per
// Design Q5). Authors needing bash-specific features ship bash in their image
// and write `bash -c '...'` as the inner script.
//
// TTY: ExecOptions.Tty is left false (zero value); the response stream is
// multiplexed (stdout + stderr framed) and stdcopy.StdCopy demuxes them.
// Setting Tty=true would produce a raw stream (no framing) — not what slice
// 4.2 wants.
//
// On ctx cancellation: a watcher goroutine closes the attach response, the
// reader goroutine exits, Exec returns (ExecResult{}, nil, ctx.Err()) with
// no leaked goroutines. Verified by Bucket 9b's ctx-cancel test (Phase 4
// design §G targets <500ms; the test asserts <5s to avoid CI flake while
// still verifying the property exists).
//
// On non-nil error: the returned channel is nil (Phase 2 invariant — see
// container/backend.go Exec doc-comment). Callers MUST check err before
// ranging over the channel.
func (b *Backend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (container.ExecResult, <-chan container.IOChunk, error) {
	r, err := b.lookupRegistered(ctx, "Exec", h)
	if err != nil {
		return container.ExecResult{}, nil, err
	}
	dockerID := r.dockerID

	execCreateResp, err := b.cli.ContainerExecCreate(ctx, dockerID, dockerContainer.ExecOptions{
		Cmd:          []string{"sh", "-c", cmd.Run},
		Env:          envMapToSlice(cmd.Env),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return container.ExecResult{}, nil, fmt.Errorf("container/docker: Exec: ContainerExecCreate: %w", err)
	}

	// ContainerExecAttach takes container.ExecAttachOptions (an alias for
	// ExecStartOptions in v28.5.2). Detach=false, Tty=false → multiplexed
	// stream that stdcopy can demux.
	attachResp, err := b.cli.ContainerExecAttach(ctx, execCreateResp.ID, dockerContainer.ExecAttachOptions{})
	if err != nil {
		// The exec instance from ContainerExecCreate stays registered on the
		// daemon (moby daemon/exec.go's registerExecCommand happens BEFORE we
		// attach, and Docker's SDK exposes no ContainerExecRemove). The orphan
		// is auto-cleaned when the parent container is Destroyed
		// (ContainerRemove(force) — slice 4.1) — bounded by container
		// lifetime, ~few hundred bytes per orphan. Acceptable.
		return container.ExecResult{}, nil, fmt.Errorf("container/docker: Exec: ContainerExecAttach: %w", err)
	}
	// HijackedResponse wraps a net.Conn + *bufio.Reader. Close() calls
	// Conn.Close() — idempotent per net.Conn convention, so the double-close
	// path (watcher on ctx-cancel + reader's defer) is safe.

	// Watcher goroutine: on ctx-cancel, close the attach response → the
	// stdcopy reader on attachResp.Reader gets EOF/ErrClosed → the reader
	// goroutine exits. The watcher itself exits when readerDone is signalled
	// (happy path) or when ctx is canceled.
	readerDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			attachResp.Close()
		case <-readerDone:
			// Reader finished naturally; reader's defer closes.
		}
	}()

	// Reader goroutine: demux stdout/stderr via stdcopy, emit one IOChunk per
	// stdcopy frame, accumulate stdout into ExecResult.Stdout. stderr is NOT
	// accumulated (spec §4.1 + Phase 2 container.ExecResult shape) — it
	// surfaces only via IOChunk{Stream:"stderr"} for the live tap.
	//
	// No mutex needed: stdcopy.StdCopy is single-threaded; chunkBuffer.Write
	// is called serially from this one goroutine. The main routine reads
	// `chunks`/`stdoutBuf.collected` AFTER `<-readerDone` (happens-before
	// via channel close), so there's no data race either.
	chunks := make([]container.IOChunk, 0, 8)
	stdoutBuf := chunkBuffer{stream: "stdout", chunks: &chunks}
	stderrBuf := chunkBuffer{stream: "stderr", chunks: &chunks}

	var readerErr error
	go func() {
		defer close(readerDone)
		defer attachResp.Close()
		_, readerErr = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, attachResp.Reader)
	}()

	// Wait for the reader to finish (either the command exited, or ctx-cancel
	// closed the response and the reader errored out). Then drain the watcher.
	<-readerDone
	<-watcherDone

	// ctx-dead dominates any readerErr.
	if err := ctx.Err(); err != nil {
		return container.ExecResult{}, nil, err
	}

	// On ctx-cancel, the watcher closes attachResp; StdCopy's src.Read returns
	// a net.ErrClosed-wrapped error — but the ctx.Err() check above already
	// handles that path. Any readerErr reaching here is a genuine transport
	// problem.
	if readerErr != nil {
		return container.ExecResult{}, nil, fmt.Errorf("container/docker: Exec: stdcopy: %w", readerErr)
	}

	// Exit code via ContainerExecInspect. (No Running-check guard: when the
	// response stream closed, the exec terminated; Docker daemon updates
	// ExecInspect synchronously. If a future SDK bug surfaces a Running=true
	// here, surface the resulting ExitCode=0 plainly rather than masking via
	// a custom error — the daemon bug deserves the loud failure.)
	inspect, err := b.cli.ContainerExecInspect(ctx, execCreateResp.ID)
	if err != nil {
		return container.ExecResult{}, nil, fmt.Errorf("container/docker: Exec: ContainerExecInspect: %w", err)
	}

	out := make(chan container.IOChunk, len(chunks))
	for _, c := range chunks {
		out <- c
	}
	close(out)

	return container.ExecResult{
		ExitCode:  inspect.ExitCode,
		Stdout:    stdoutBuf.collected,
		AWFOutput: nil, // dispatcher reads AWF_OUTPUT tempfile via CaptureFiles (Design Q1).
	}, out, nil
}

// chunkBuffer is an io.Writer that stdcopy.StdCopy writes into. Each Write
// emits one IOChunk (stream-tagged) into *chunks and (for stdout) accumulates
// into `collected` for ExecResult.Stdout.
//
// No mutex: stdcopy is single-threaded; both chunkBuffers receive Writes from
// the same goroutine.
type chunkBuffer struct {
	stream    string
	chunks    *[]container.IOChunk
	collected []byte
}

// Write defensive-copies p. stdcopy.StdCopy passes slices of a single
// per-call read buffer that it shifts and reuses for the next frame
// (see StdCopy's inner loop: copy(buf, buf[frameSize+stdWriterPrefixLen:])).
// Without the copy, later frames would overwrite the bytes of earlier chunks.
func (cb *chunkBuffer) Write(p []byte) (int, error) {
	dup := make([]byte, len(p))
	copy(dup, p)
	*cb.chunks = append(*cb.chunks, container.IOChunk{Stream: cb.stream, Data: dup})
	if cb.stream == "stdout" {
		cb.collected = append(cb.collected, dup...)
	}
	return len(p), nil
}

// envMapToSlice converts the map[string]string from Cmd.Env into the
// []string the Docker SDK expects ("KEY=value" form). Deterministic
// (sort.Strings) so test goldens are stable.
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
