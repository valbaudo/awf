package container_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/backendtest"
)

// Compile-time check: Fake satisfies Backend. Catches signature drift the
// moment a method signature changes.
var _ container.Backend = (*container.Fake)(nil)

// Interface conformance via the shared contract helper.
func TestFakeRunBasicContract(t *testing.T) {
	backendtest.RunBasicContract(t, container.NewFake())
}

// CopyTo round-trip via the shared contract helper.
func TestFakeCopyToContract(t *testing.T) {
	backendtest.RunCopyToContract(t, container.NewFake())
}

// Behavior tests below are fake-specific (they use ProgramExec / WriteFile,
// which aren't on the Backend interface). Phase 4 Docker will have its own
// equivalents (sentinel commands / docker cp) in docker_test.go.

func TestFakeExecScriptedResult(t *testing.T) {
	f := container.NewFake()
	ctx := context.Background()
	h, err := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = f.Destroy(ctx, h) }()

	want := container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"web_exploitable":true}`),
		Stdout:    []byte("hello\n"),
	}
	cmd := container.Cmd{Run: "./triage.sh"}
	f.ProgramExec(cmd.Run, want, nil)

	_, resultCh, err := f.Exec(ctx, h, cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := <-resultCh
	if got.ExitCode != want.ExitCode {
		t.Errorf("ExitCode = %d, want %d", got.ExitCode, want.ExitCode)
	}
	if string(got.AWFOutput) != string(want.AWFOutput) {
		t.Errorf("AWFOutput = %q, want %q", got.AWFOutput, want.AWFOutput)
	}
	if string(got.Stdout) != string(want.Stdout) {
		t.Errorf("Stdout = %q, want %q", got.Stdout, want.Stdout)
	}
}

func TestFakeExecStreamsChunks(t *testing.T) {
	f := container.NewFake()
	ctx := context.Background()
	h, err := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = f.Destroy(ctx, h) }()

	wantChunks := []container.IOChunk{
		{Stream: "stdout", Data: []byte("step starting\n")},
		{Stream: "stdout", Data: []byte("step done\n")},
		{Stream: "stderr", Data: []byte("warning: deprecated\n")},
	}
	cmd := container.Cmd{Run: "./noisy.sh"}
	f.ProgramExec(cmd.Run, container.ExecResult{ExitCode: 0}, wantChunks)

	ch, _, err := f.Exec(ctx, h, cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var got []container.IOChunk
	for c := range ch {
		got = append(got, c)
	}
	if len(got) != len(wantChunks) {
		t.Fatalf("got %d chunks, want %d", len(got), len(wantChunks))
	}
	for i, w := range wantChunks {
		if got[i].Stream != w.Stream || string(got[i].Data) != string(w.Data) {
			t.Errorf("chunks[%d] = {%q, %q}, want {%q, %q}",
				i, got[i].Stream, got[i].Data, w.Stream, w.Data)
		}
	}
}

func TestFakeExecChannelClosedWithNoChunks(t *testing.T) {
	// Contract: the channel is buffered and pre-closed even when no chunks
	// were programmed. Live tap's `for range ch` must exit cleanly.
	f := container.NewFake()
	ctx := context.Background()
	h, _ := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	defer func() { _ = f.Destroy(ctx, h) }()
	f.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	ch, _, _ := f.Exec(ctx, h, container.Cmd{Run: "true"})
	if _, ok := <-ch; ok {
		t.Errorf("channel returned a value; want closed-empty")
	}
}

func TestFakeExecUnscriptedErrors(t *testing.T) {
	// Unprogrammed Cmd.Run is a hard error — silent zero-value would mask
	// slice 2.4 dispatcher bugs (e.g., wrong-cmd misroutes).
	f := container.NewFake()
	ctx := context.Background()
	h, _ := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	defer func() { _ = f.Destroy(ctx, h) }()
	if _, _, err := f.Exec(ctx, h, container.Cmd{Run: "never-programmed"}); err == nil {
		t.Errorf("Exec on unprogrammed cmd returned nil; want error")
	}
}

func TestFakeExecUnknownHandleErrors(t *testing.T) {
	f := container.NewFake()
	f.ProgramExec("noop", container.ExecResult{}, nil)
	if _, _, err := f.Exec(context.Background(), container.Handle{ID: "ghost"}, container.Cmd{Run: "noop"}); err == nil {
		t.Errorf("Exec on unknown handle returned nil; want error")
	}
}

func TestFakeCaptureFilesRoundTrip(t *testing.T) {
	f := container.NewFake()
	ctx := context.Background()
	h, _ := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	defer func() { _ = f.Destroy(ctx, h) }()

	wantA := []byte("alpha contents\n")
	wantB := []byte(`{"results":[1,2,3]}`)
	if err := f.WriteFile(h, "/out/a.txt", wantA); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := f.WriteFile(h, "/out/b.json", wantB); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	got, err := f.CaptureFiles(ctx, h, []string{"/out/a.txt", "/out/b.json"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d files, want 2", len(got))
	}
	// Order MUST match input.
	if got[0].Path != "/out/a.txt" || string(got[0].Content) != string(wantA) {
		t.Errorf("got[0] = {%q, %q}", got[0].Path, got[0].Content)
	}
	if got[1].Path != "/out/b.json" || string(got[1].Content) != string(wantB) {
		t.Errorf("got[1] = {%q, %q}", got[1].Path, got[1].Content)
	}
}

func TestFakeCaptureFilesMissingPathErrors(t *testing.T) {
	// All-or-nothing: a single missing path errors the whole call. Slice 2.4
	// relies on this so a half-populated NodeCompletedData.Files can't slip
	// into the commit boundary.
	f := container.NewFake()
	ctx := context.Background()
	h, _ := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	defer func() { _ = f.Destroy(ctx, h) }()
	_ = f.WriteFile(h, "/out/exists.txt", []byte("here"))
	if _, err := f.CaptureFiles(ctx, h, []string{"/out/exists.txt", "/out/missing.txt"}); err == nil {
		t.Errorf("CaptureFiles with missing path returned nil; want error")
	}
}

func TestFakeCaptureFilesUnknownHandleErrors(t *testing.T) {
	f := container.NewFake()
	if _, err := f.CaptureFiles(context.Background(), container.Handle{ID: "ghost"}, nil); err == nil {
		t.Errorf("CaptureFiles on unknown handle returned nil; want error")
	}
}

func TestFakeWriteFileUnknownHandleErrors(t *testing.T) {
	f := container.NewFake()
	if err := f.WriteFile(container.Handle{ID: "ghost"}, "/x", []byte("y")); err == nil {
		t.Errorf("WriteFile on unknown handle returned nil; want error")
	}
}

func TestFakeSnapshotIsUnsupported(t *testing.T) {
	// Direct call documents intent at the package level. The contract test
	// covers the routing too; both stay green so a programmer touching either
	// surface gets a loud failure.
	f := container.NewFake()
	ctx := context.Background()
	h, _ := f.Create(ctx, container.ContainerSpec{Name: "workspace"})
	defer func() { _ = f.Destroy(ctx, h) }()
	if _, err := f.Snapshot(ctx, h); !errors.Is(err, container.ErrUnsupported) {
		t.Errorf("Snapshot: err = %v, want errors.Is(_, ErrUnsupported)", err)
	}
}

func TestFakeFailExecAfterN(t *testing.T) {
	for _, k := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			f := container.NewFake()
			f.ProgramExec("noop", container.ExecResult{ExitCode: 0}, nil)
			ctx := context.Background()
			h, err := f.Create(ctx, container.ContainerSpec{Name: "lab"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			f.FailExecAfterN(k)

			// First k calls succeed.
			for i := 0; i < k; i++ {
				if _, _, err := f.Exec(ctx, h, container.Cmd{Run: "noop"}); err != nil {
					t.Fatalf("call #%d before fault: %v", i, err)
				}
			}
			// Call #k fails.
			if _, _, err := f.Exec(ctx, h, container.Cmd{Run: "noop"}); err == nil {
				t.Errorf("call #%d did not trigger fault", k)
			}
			// Call #(k+1) succeeds — one-shot semantic.
			if _, _, err := f.Exec(ctx, h, container.Cmd{Run: "noop"}); err != nil {
				t.Errorf("call #%d after fault returned err=%v; want nil (one-shot)", k+1, err)
			}
		})
	}
}

func TestFakeFailCaptureAfterN(t *testing.T) {
	for _, k := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			f := container.NewFake()
			ctx := context.Background()
			h, err := f.Create(ctx, container.ContainerSpec{Name: "lab"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := f.WriteFile(h, "/out/a", []byte("hi")); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			f.FailCaptureAfterN(k)

			for i := 0; i < k; i++ {
				if _, err := f.CaptureFiles(ctx, h, []string{"/out/a"}); err != nil {
					t.Fatalf("call #%d before fault: %v", i, err)
				}
			}
			if _, err := f.CaptureFiles(ctx, h, []string{"/out/a"}); err == nil {
				t.Errorf("call #%d did not trigger fault", k)
			}
			// One-shot: call #(k+1) succeeds.
			if _, err := f.CaptureFiles(ctx, h, []string{"/out/a"}); err != nil {
				t.Errorf("call #%d after fault returned err=%v; want nil (one-shot)", k+1, err)
			}
		})
	}
}

func TestFakeExecHonorsContextCancel(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("./should-not-run.sh", container.ExecResult{ExitCode: 0}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := f.Exec(ctx, h, container.Cmd{Run: "./should-not-run.sh"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec with cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestFakeCaptureFilesHonorsContextCancel(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	_ = f.WriteFile(h, "/out/x", []byte("data"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.CaptureFiles(ctx, h, []string{"/out/x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CaptureFiles with cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestFakeRecordsCallsHistory(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("./run.sh", container.ExecResult{ExitCode: 0}, nil)
	cmd1 := container.Cmd{Run: "./run.sh", Env: map[string]string{"K": "v1"}}
	_, _, _ = f.Exec(context.Background(), h, cmd1)
	cmd2 := container.Cmd{Run: "./run.sh", Env: map[string]string{"K": "v2", "AWF_IDEMPOTENCY_KEY": "abc"}}
	_, _, _ = f.Exec(context.Background(), h, cmd2)

	if len(f.Calls) != 2 {
		t.Fatalf("len(Calls) = %d, want 2", len(f.Calls))
	}
	if f.Calls[0].Env["K"] != "v1" || f.Calls[1].Env["K"] != "v2" {
		t.Errorf("Calls didn't record both: %+v", f.Calls)
	}
	if f.Calls[1].Env["AWF_IDEMPOTENCY_KEY"] != "abc" {
		t.Errorf("Calls[1] missing AWF_IDEMPOTENCY_KEY: %+v", f.Calls[1].Env)
	}
	// Defensive-copy check: mutating the caller's map post-Exec must NOT corrupt
	// the recorded Cmd. (Matches ProgramExec's defensive-copy discipline.)
	cmd2.Env["K"] = "MUTATED"
	if f.Calls[1].Env["K"] != "v2" {
		t.Errorf("Fake aliased caller's Env map: Calls[1].Env[K] = %q after caller mutation, want %q", f.Calls[1].Env["K"], "v2")
	}
}

func TestFakeClearFaultResetsBothHooks(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.ProgramExec("./x.sh", container.ExecResult{ExitCode: 0}, nil)
	fake.FailExecAfterN(0)
	fake.FailCaptureAfterN(0)
	fake.ClearFault()
	if _, _, err := fake.Exec(context.Background(), h, container.Cmd{Run: "./x.sh"}); err != nil {
		t.Errorf("Exec after ClearFault: %v (want nil)", err)
	}
	// CaptureFiles on empty paths returns an empty slice without erroring.
	if _, err := fake.CaptureFiles(context.Background(), h, nil); err != nil {
		t.Errorf("CaptureFiles after ClearFault: %v (want nil)", err)
	}
}

func TestFakeConcurrentExec(t *testing.T) {
	// Phase 3 slice 3.2 — parallel branches dispatch to distinct containers
	// (spec §5.4, AWF1010 validator rule), but the harness backs every
	// container with the same *Fake instance (conformance/harness.go).
	// Without per-method sync, f.Calls + f.execCalls + f.execTable race.
	f := container.NewFake()
	ctx := context.Background()
	const N = 8
	handles := make([]container.Handle, N)
	for i := 0; i < N; i++ {
		h, err := f.Create(ctx, container.ContainerSpec{Name: fmt.Sprintf("c-%d", i)})
		if err != nil {
			t.Fatalf("Create c-%d: %v", i, err)
		}
		handles[i] = h
		f.ProgramExec(fmt.Sprintf("./run-%d.sh", i), container.ExecResult{ExitCode: 0}, nil)
	}
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := container.Cmd{Run: fmt.Sprintf("./run-%d.sh", i)}
			ch, resultCh, err := f.Exec(ctx, handles[i], cmd)
			if err != nil {
				t.Errorf("concurrent Exec %d: %v", i, err)
				return
			}
			for range ch {
			}
			res := <-resultCh
			if res.ExitCode != 0 {
				t.Errorf("Exec %d: ExitCode=%d, want 0", i, res.ExitCode)
			}
		}()
	}
	wg.Wait()
	if len(f.Calls) != N {
		t.Errorf("Calls len = %d, want %d", len(f.Calls), N)
	}
}

func TestFake_Exec_StreamingContract(t *testing.T) {
	f := container.NewFake()
	h, err := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.ProgramExec("echo hi", container.ExecResult{ExitCode: 0, Stdout: []byte("hi\n")}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("hi\n")},
	})
	chunks, result, err := f.Exec(context.Background(), h, container.Cmd{Run: "echo hi"})
	if err != nil {
		t.Fatalf("Exec err: %v", err)
	}
	if chunks == nil {
		t.Fatal("chunks channel is nil; expected non-nil on success")
	}
	if result == nil {
		t.Fatal("result channel is nil; expected non-nil on success")
	}
	var gotChunks []container.IOChunk
	for c := range chunks {
		gotChunks = append(gotChunks, c)
	}
	if len(gotChunks) != 1 || string(gotChunks[0].Data) != "hi\n" {
		t.Errorf("chunks = %+v; want one stdout chunk with hi\\n", gotChunks)
	}
	r, ok := <-result
	if !ok {
		t.Fatal("result channel closed without emitting")
	}
	if r.ExitCode != 0 || string(r.Stdout) != "hi\n" {
		t.Errorf("ExecResult = %+v; want ExitCode 0 + Stdout hi\\n", r)
	}
	// Receiving again should observe close.
	if _, ok := <-result; ok {
		t.Error("result channel did not close after delivering value")
	}
}

func TestFake_Exec_ErrorReturnsNilChannels(t *testing.T) {
	f := container.NewFake()
	// Unknown handle: triggers the err path.
	chunks, result, err := f.Exec(context.Background(), container.Handle{ID: "missing"}, container.Cmd{Run: "x"})
	if err == nil {
		t.Fatal("err nil; want non-nil for unknown handle")
	}
	if chunks != nil {
		t.Errorf("chunks not nil on err: %v", chunks)
	}
	if result != nil {
		t.Errorf("result not nil on err: %v", result)
	}
}

func TestFakeProgramExecDefensiveCopy(t *testing.T) {
	// Mirror state.InMemoryBlobs.TestInMemoryBlobsDefensiveCopy. A caller that
	// mutates the slices passed to ProgramExec after it returns must NOT
	// corrupt what Exec subsequently returns. Matches the discipline already
	// applied to WriteFile and CaptureFiles.
	f := container.NewFake()
	ctx := context.Background()
	h, err := f.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = f.Destroy(ctx, h) }()

	awfOut := []byte(`{"ok":true}`)
	stdout := []byte("hello\n")
	chunkData := []byte("chunk-payload")
	chunks := []container.IOChunk{{Stream: "stdout", Data: chunkData}}

	f.ProgramExec("noop", container.ExecResult{
		ExitCode:  0,
		AWFOutput: awfOut,
		Stdout:    stdout,
	}, chunks)

	// Mutate ALL inputs after ProgramExec returns. The fake's stored copies
	// must be unaffected.
	awfOut[0] = 'X'
	stdout[0] = 'X'
	chunkData[0] = 'X'
	chunks[0].Stream = "stderr" // also mutate the chunks slice header

	ch, resultCh, err := f.Exec(ctx, h, container.Cmd{Run: "noop"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := <-resultCh
	if string(got.AWFOutput) != `{"ok":true}` {
		t.Errorf("AWFOutput leaked caller mutation: got %q", got.AWFOutput)
	}
	if string(got.Stdout) != "hello\n" {
		t.Errorf("Stdout leaked caller mutation: got %q", got.Stdout)
	}
	c, ok := <-ch
	if !ok {
		t.Fatalf("chunk channel closed before any chunk")
	}
	if c.Stream != "stdout" {
		t.Errorf("chunk Stream leaked caller mutation: got %q, want \"stdout\"", c.Stream)
	}
	if string(c.Data) != "chunk-payload" {
		t.Errorf("chunk Data leaked caller mutation: got %q", c.Data)
	}
}
