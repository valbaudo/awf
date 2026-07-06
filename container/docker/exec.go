package docker

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/valbaudo/awf/container"
)

// Exec runs cmd inside the container the handle references. Slice 5.3
// streaming contract:
//
//   - chunks: emits each stdout/stderr frame live as stdcopy.StdCopy
//     demuxes them; closes when both pipes drain.
//   - result: receives one ExecResult AFTER chunks closes (ExitCode,
//     accumulated Stdout, Err). ExecResult.Err carries transport-class
//     errors (ctx-cancel, stdcopy mid-stream failure, ContainerExecInspect
//     failure) so callers learn about them without the err return swallowing
//     them before the chunks drain.
//
// ExecResult.AWFOutput is left nil — the dispatcher reads the AWF_OUTPUT
// tempfile via CaptureFiles per the Phase 4 design §B contract (slice 4.2
// Design Q1). A future Backend may populate it; the dispatcher handles both.
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
// reader goroutine exits, chunks closes, and the result carries
// ExecResult{Err: ctx.Err()}.
//
// On non-nil error return: the "launch / transport" error class — the
// command couldn't be started at all. BOTH channels are nil; callers must
// check err before ranging or receiving.
func (b *Backend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	r, err := b.lookupRegistered(ctx, "Exec", h)
	if err != nil {
		return nil, nil, err
	}
	switch r.kind {
	case kindImage:
		return b.execImage(ctx, r.dockerID, cmd)
	case kindCompose:
		return b.execCompose(ctx, h, r, cmd)
	default:
		return nil, nil, fmt.Errorf("container/docker: Exec: unknown handle kind %q (engine bug)", r.kind)
	}
}

// execImage is the core Exec implementation for image-mode containers.
// It is also called by execCompose after container ID resolution.
func (b *Backend) execImage(ctx context.Context, dockerID string, cmd container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	// Wrap the command so its process tree can be reaped on ctx-cancel. The
	// wrapping `sh` writes its own PID (the tree root) to a per-exec pidfile,
	// then runs cmd.Run in the FOREGROUND — stdin, stdout, and the exit code are
	// unchanged (`echo` redirects to the file, and a ;-list's exit status is
	// cmd.Run's). On timeout the watcher reaps root + PPID-reachable descendants
	// (killExecTree); without it, docker exec has no per-exec kill and a stalled
	// child lingers in the keepalive container until teardown.
	pidfile := fmt.Sprintf("/tmp/.awf-exec-%d.pid", b.execSeq.Add(1))
	wrapped := "echo $$ > '" + pidfile + "'; " + cmd.Run
	execCreateResp, err := b.cli.ContainerExecCreate(ctx, dockerID, dockerContainer.ExecOptions{
		Cmd:          []string{"sh", "-c", wrapped},
		Env:          envMapToSlice(cmd.Env),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("container/docker: Exec: ContainerExecCreate: %w", err)
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
		return nil, nil, fmt.Errorf("container/docker: Exec: ContainerExecAttach: %w", err)
	}
	// HijackedResponse wraps a net.Conn + *bufio.Reader. Close() calls
	// Conn.Close() — idempotent per net.Conn convention, so the double-close
	// path (watcher on ctx-cancel + reader's defer) is safe.

	chunks := make(chan container.IOChunk, 64)
	result := make(chan container.ExecResult, 1)

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
			// Reap the step's process tree (best-effort, bounded) — docker exec
			// has no per-exec kill, so otherwise a stalled child keeps running in
			// the keepalive container. Detached so it never delays result
			// delivery; attachResp.Close() below unblocks the reader immediately.
			go b.killExecTree(dockerID, pidfile)
			attachResp.Close()
		case <-readerDone:
			// Reader finished naturally; reader's defer closes.
		}
	}()

	// Reader goroutine: demux stdout/stderr via stdcopy. Each stdcopy frame
	// becomes one IOChunk sent live on `chunks`. stdout writes also append
	// to stdoutAccum for ExecResult.Stdout — stderr is NOT accumulated
	// (spec §4.1 + Phase 2 container.ExecResult shape; it surfaces only via
	// IOChunk{Stream:"stderr"}). The accum mutex is required only because
	// the main goroutine reads stdoutAccum after the reader goroutine
	// finishes; stdcopy itself is single-threaded.
	var stdoutMu sync.Mutex
	var stdoutAccum []byte
	stdoutWriter := streamingWriter{stream: "stdout", out: chunks, accum: &stdoutAccum, mu: &stdoutMu}
	stderrWriter := streamingWriter{stream: "stderr", out: chunks, accum: nil}

	go func() {
		// Capture readerErr from stdcopy.StdCopy. Phase 4 surfaced transport
		// errors (daemon disconnect, malformed multiplex frames) via this
		// error; slice 5.3 preserves it via ExecResult.Err.
		_, readerErr := stdcopy.StdCopy(stdoutWriter, stderrWriter, attachResp.Reader)
		// Close readerDone EXPLICITLY (not via defer) BEFORE waiting on
		// watcherDone, otherwise we deadlock: defer-based close would fire
		// only after this goroutine returns, but it can't return because
		// it's blocked here; meanwhile the watcher's select is blocked
		// waiting for readerDone to fire. Surfaced in CI integ runs against
		// real Docker (TestE2E_ComposeContainerStepDispatch,
		// TestConformanceDockerBackend/bucket9/streamed_exec_demux,
		// TestCLIRunDockerBackendPauseResumeRoundTrip).
		close(readerDone)
		<-watcherDone
		attachResp.Close()

		// Exit code via ContainerExecInspect. (No Running-check guard: when
		// the response stream closed, the exec terminated; Docker daemon
		// updates ExecInspect synchronously. If a future SDK bug surfaces a
		// Running=true here, surface the resulting ExitCode=0 plainly rather
		// than masking via a custom error — the daemon bug deserves the
		// loud failure.)
		exitCode := 0
		var inspectErr error
		if inspect, err := b.cli.ContainerExecInspect(ctx, execCreateResp.ID); err == nil {
			exitCode = inspect.ExitCode
		} else {
			inspectErr = fmt.Errorf("container/docker: Exec: ContainerExecInspect: %w", err)
		}
		close(chunks)
		stdoutMu.Lock()
		out := append([]byte(nil), stdoutAccum...)
		stdoutMu.Unlock()

		// Error precedence (preserved from pre-slice-5.3): ctx.Err()
		// dominates any readerErr (on ctx-cancel the watcher closes
		// attachResp and stdcopy returns a net.ErrClosed-wrapped error
		// that is uninteresting), then a genuine readerErr (transport
		// failure), then inspectErr.
		var resErr error
		switch {
		case ctx.Err() != nil:
			resErr = ctx.Err()
		case readerErr != nil:
			resErr = fmt.Errorf("container/docker: Exec: stdcopy: %w", readerErr)
		case inspectErr != nil:
			resErr = inspectErr
		}
		result <- container.ExecResult{
			ExitCode:  exitCode,
			Stdout:    out,
			AWFOutput: nil, // dispatcher reads AWF_OUTPUT tempfile via CaptureFiles (Design Q1).
			Err:       resErr,
		}
		close(result)
	}()

	return chunks, result, nil
}

// killExecTree reaps a timed-out exec's process tree inside the container.
// Docker exposes no per-exec kill, so on ctx-cancel we run a fresh exec that
// reads the wrapping shell's PID from the per-exec pidfile, computes the
// transitive closure of its descendants from /proc/<pid>/status (busybox-safe:
// no procps, no fragile /proc/stat comm-field parsing), and SIGKILLs the set.
// Best-effort and bounded: a background context (the step ctx is already
// cancelled), a short timeout, all errors ignored; Detach:true so the daemon
// runs it independently of this call.
//
// Limitation: a process that reparents to init (an intermediate parent exited)
// or double-forks to daemonize loses its PPID link and is not reaped here; such
// stragglers are cleaned up when the container is Destroyed. This reaps the real
// case — a stalled agent CLI / script and its still-parented children.
func (b *Backend) killExecTree(dockerID, pidfile string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := b.cli.ContainerExecCreate(ctx, dockerID, dockerContainer.ExecOptions{
		Cmd: []string{"sh", "-c", killScript(pidfile)},
	})
	if err != nil {
		return
	}
	_ = b.cli.ContainerExecStart(ctx, resp.ID, dockerContainer.ExecStartOptions{Detach: true})
}

// killScript is the POSIX/busybox shell that reaps the tree rooted at the PID in
// pidfile: it builds the descendant set as a transitive closure over PPid, then
// SIGKILLs the whole set at once. Enumerate-then-kill is deliberate — killing
// incrementally would reparent not-yet-seen children to init and break the walk.
// pidfile is an AWF-controlled path (no user input), so single-quoting is safe.
func killScript(pidfile string) string {
	return `L=$(cat '` + pidfile + `' 2>/dev/null); [ -n "$L" ] || exit 0
t=" $L "
c=1
while [ "$c" = 1 ]; do
  c=0
  for s in /proc/[0-9]*/status; do
    p=${s#/proc/}; p=${p%/status}
    case "$t" in *" $p "*) continue ;; esac
    pp=$(sed -n 's/^PPid:[[:space:]]*//p' "$s" 2>/dev/null)
    [ -n "$pp" ] || continue
    case "$t" in *" $pp "*) t="$t$p "; c=1 ;; esac
  done
done
kill -KILL $t 2>/dev/null
rm -f '` + pidfile + `' 2>/dev/null`
}

// streamingWriter is an io.Writer that emits one IOChunk per Write to the
// shared chunks channel. (Replaces slice 4.2's accumulate-then-burst
// chunkBuffer.) stdout writes also append to accum for ExecResult.Stdout.
//
// stdcopy.StdCopy passes slices of a single per-call read buffer that it
// shifts and reuses for the next frame; we defensive-copy p before sending
// so later frames don't overwrite the bytes of earlier chunks.
type streamingWriter struct {
	stream string
	out    chan<- container.IOChunk
	accum  *[]byte
	mu     *sync.Mutex
}

func (w streamingWriter) Write(p []byte) (int, error) {
	data := make([]byte, len(p))
	copy(data, p)
	w.out <- container.IOChunk{Stream: w.stream, Data: data}
	if w.accum != nil {
		w.mu.Lock()
		*w.accum = append(*w.accum, data...)
		w.mu.Unlock()
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
