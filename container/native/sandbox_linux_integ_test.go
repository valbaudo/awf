//go:build integ && linux

package native_test

// Linux live isolation integration tests.
//
// cve-runner-pending: these tests require a Linux host (bwrap or Landlock ABI),
// run as non-root (for bwrap user namespaces), and the awf binary in PATH.
// Run on cve-runner (ssh root@172.28.0.199 → pct exec 112) via:
//
//	go test -v -tags integ ./container/native/ -run TestSandboxInteg
//
// Tests assert:
//   - A step writing to host HOME (~/.factory/x) is DENIED.
//   - A step writing to the scratch dir (<scratch>/x) succeeds.
//   - A step reading ~/.factory is permitted (RO access granted).
//
// Each test constructs a real Backend with WithSandbox(true), Creates a handle,
// execs a step, and checks the exit code + stdout.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestSandboxInteg_BwrapWriteHostDenied asserts that a bwrap-sandboxed step
// cannot write to host HOME. Requires bwrap(1) in PATH.
//
// cve-runner-pending.
func TestSandboxInteg_BwrapWriteHostDenied(t *testing.T) {
	requireBwrap(t)

	hostHome := os.Getenv("HOME")
	targetFile := filepath.Join(hostHome, ".factory", "x")

	workdir := t.TempDir()
	b, err := native.New(workdir, native.WithSandbox(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-bwrap-deny"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	// Try to write to host HOME — must fail (no-such-file or permission denied)
	// because HOME is a tmpfs overlay in the sandbox.
	cmd := container.Cmd{
		Run: "echo forbidden > " + targetFile + " 2>&1; echo exit:$?",
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh

	// The target file must NOT exist on the real host.
	if _, statErr := os.Stat(targetFile); statErr == nil {
		t.Errorf("SECURITY: bwrap sandbox allowed writing to host path %s", targetFile)
		os.Remove(targetFile) // clean up to avoid leaving evidence
	}

	// The shell command itself should have exited non-zero (write failed).
	if result.ExitCode == 0 {
		t.Errorf("step exited 0 despite sandbox write denial; stdout=%q", result.Stdout)
	}
}

// TestSandboxInteg_BwrapWriteScratchOK asserts that a bwrap-sandboxed step
// CAN write to its scratch dir (the per-run workdir). Requires bwrap(1).
//
// cve-runner-pending.
func TestSandboxInteg_BwrapWriteScratchOK(t *testing.T) {
	requireBwrap(t)

	workdir := t.TempDir()
	b, err := native.New(workdir, native.WithSandbox(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-bwrap-scratch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	// Write a file to CWD (which is the scratch dir inside bwrap).
	cmd := container.Cmd{
		Run: "echo ok > output.txt && cat output.txt",
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh

	if result.ExitCode != 0 {
		t.Errorf("scratch write failed: exit=%d stdout=%q", result.ExitCode, result.Stdout)
	}
	if !strings.Contains(string(result.Stdout), "ok") {
		t.Errorf("stdout %q missing \"ok\"", result.Stdout)
	}
}

// TestSandboxInteg_LandlockWriteHostDenied asserts that a landlock-trampoline-
// sandboxed step cannot write to host HOME. Requires Linux kernel Landlock ABI ≥1
// and NO bwrap in PATH (so the trampoline path is exercised).
//
// cve-runner-pending.
func TestSandboxInteg_LandlockWriteHostDenied(t *testing.T) {
	requireLandlock(t)
	requireNoBwrap(t) // force trampoline path

	hostHome := os.Getenv("HOME")
	targetFile := filepath.Join(hostHome, ".factory", "x")

	workdir := t.TempDir()
	b, err := native.New(workdir, native.WithSandbox(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-ll-deny"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	cmd := container.Cmd{
		// A bare redirect, with NO trailing `echo exit:$?`: that trailing command
		// would mask the write's failure and make the step exit 0 even when the
		// write is denied. With just the redirect, a Landlock-denied open(2) aborts
		// the command and sh exits non-zero — the signal this test asserts on.
		Run: "echo forbidden > " + targetFile,
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh

	if _, statErr := os.Stat(targetFile); statErr == nil {
		t.Errorf("SECURITY: landlock trampoline allowed writing to host path %s", targetFile)
		os.Remove(targetFile)
	}
	if result.ExitCode == 0 {
		t.Errorf("step exited 0 despite landlock write denial (redirect to %s should fail)", targetFile)
	}
}

// TestSandboxInteg_LandlockReadHomeOK asserts that a sandboxed step CAN read
// ~/.factory (the cred dir is in RODirs). Requires Landlock ABI ≥1.
//
// cve-runner-pending.
func TestSandboxInteg_LandlockReadHomeOK(t *testing.T) {
	requireLandlock(t)
	requireNoBwrap(t)

	hostHome := os.Getenv("HOME")
	factoryDir := filepath.Join(hostHome, ".factory")
	// Create ~/.factory/marker so the step can cat it.
	if err := os.MkdirAll(factoryDir, 0o755); err != nil {
		t.Skipf("cannot create ~/.factory: %v", err)
	}
	markerPath := filepath.Join(factoryDir, "marker")
	if err := os.WriteFile(markerPath, []byte("readable\n"), 0o644); err != nil {
		t.Skipf("cannot write marker: %v", err)
	}
	defer os.Remove(markerPath)

	workdir := t.TempDir()
	b, err := native.New(workdir, native.WithSandbox(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-ll-ro"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	cmd := container.Cmd{
		Run: "cat " + markerPath,
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh

	if result.ExitCode != 0 {
		t.Errorf("read of RO-allowed file failed: exit=%d stdout=%q", result.ExitCode, result.Stdout)
	}
	if !strings.Contains(string(result.Stdout), "readable") {
		t.Errorf("stdout %q missing expected content", result.Stdout)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not in PATH; skipping bwrap integ test")
	}
}

func requireNoBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err == nil {
		t.Skip("bwrap is in PATH; skipping landlock-trampoline path test (bwrap takes priority)")
	}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	// Probe the kernel Landlock ABI without applying rules.
	if _, err := llsyscall.LandlockGetABIVersion(); err != nil {
		t.Skipf("Landlock ABI not available: %v", err)
	}
}
