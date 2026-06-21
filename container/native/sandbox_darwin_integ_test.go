//go:build integ && darwin

package native_test

// macOS live isolation integration tests for sandbox-exec.
//
// Verifiable on this host — no container, no root, sandbox-exec ships with macOS.
// Run via:
//
//	go test -v -tags integ ./container/native/ -run TestSandboxDarwinInteg
//
// Tests assert:
//   - A step writing to $HOME/.factory/x is DENIED (non-zero exit / file absent on host).
//   - A step writing to <scratch>/x succeeds (exit 0 + content readable).
//   - A step reading $HOME/.config (any existing readable path) succeeds (exit 0).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// requireSandboxExec skips the test if sandbox-exec is not in PATH.
func requireSandboxExec(t *testing.T) {
	t.Helper()
	// sandbox-exec ships with macOS; absence is essentially impossible,
	// but we guard as the brief requires.
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not found at /usr/bin/sandbox-exec; skipping macOS integ test")
	}
}

// newSandboxedBackend returns a Backend with sandbox enabled, using a fresh
// temp workdir. The caller is responsible for cleanup.
func newSandboxedBackend(t *testing.T) *native.Backend {
	t.Helper()
	workdir := t.TempDir()
	b, err := native.New(workdir, native.WithSandbox(true))
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	return b
}

// TestSandboxDarwinInteg_WriteHostDenied asserts that a sandboxed step cannot
// write to $HOME/.factory/x — the file must not appear on the real host.
func TestSandboxDarwinInteg_WriteHostDenied(t *testing.T) {
	requireSandboxExec(t)

	hostHome := os.Getenv("HOME")
	if hostHome == "" {
		t.Skip("HOME not set")
	}
	targetDir := filepath.Join(hostHome, ".factory")
	targetFile := filepath.Join(targetDir, "awf-sandbox-integ-test-x")
	// Ensure the target file does not linger from a prior run.
	os.Remove(targetFile) //nolint:errcheck

	b := newSandboxedBackend(t)
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-darwin-deny"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	// Try to mkdir + write to host HOME/.factory/x — the SBPL denies writes
	// outside SCRATCH and TMPDIR, so this must fail.
	cmd := container.Cmd{
		Run: "mkdir -p " + targetDir + " && echo forbidden > " + targetFile + " 2>&1; echo exit:$?",
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh
	t.Logf("write-host-denied: exit=%d stdout=%q", result.ExitCode, result.Stdout)

	// Primary security invariant: the file must NOT exist on the real host.
	if _, statErr := os.Stat(targetFile); statErr == nil {
		t.Errorf("SECURITY: sandbox-exec allowed writing to host path %s", targetFile)
		os.Remove(targetFile) //nolint:errcheck
	}

	// Secondary check: stdout should contain "exit:1" indicating the shell
	// reported a non-zero exit from the write attempt. The overall process exits
	// 0 because "echo exit:$?" is the last command — we verify the inner failure
	// via the printed marker rather than the outer exit code.
	if !strings.Contains(string(result.Stdout), "exit:1") {
		t.Errorf("expected stdout to contain \"exit:1\" (write denied); got %q", result.Stdout)
	}
}

// TestSandboxDarwinInteg_WriteScratchOK asserts that a sandboxed step CAN
// write to its scratch dir (the per-run workdir mapped via SCRATCH param).
func TestSandboxDarwinInteg_WriteScratchOK(t *testing.T) {
	requireSandboxExec(t)

	b := newSandboxedBackend(t)
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-darwin-scratch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	// Write to CWD (= scratch dir); should succeed.
	cmd := container.Cmd{
		Run: "echo ok > output.txt && cat output.txt",
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh
	t.Logf("write-scratch-ok: exit=%d stdout=%q", result.ExitCode, result.Stdout)

	if result.ExitCode != 0 {
		t.Errorf("scratch write failed: exit=%d stdout=%q", result.ExitCode, result.Stdout)
	}
	if !strings.Contains(string(result.Stdout), "ok") {
		t.Errorf("stdout %q missing \"ok\"", result.Stdout)
	}
}

// TestSandboxDarwinInteg_ReadConfigOK asserts that a sandboxed step CAN read
// an existing host path (reads are permitted by (allow default)).
func TestSandboxDarwinInteg_ReadConfigOK(t *testing.T) {
	requireSandboxExec(t)

	hostHome := os.Getenv("HOME")
	if hostHome == "" {
		t.Skip("HOME not set")
	}
	// Use $HOME itself as the readable target; it always exists.
	readTarget := hostHome

	b := newSandboxedBackend(t)
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "integ-darwin-read"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Destroy(ctx, h) //nolint:errcheck

	// Use /dev/null for discard — the updated SBPL permits writes to /dev/null.
	cmd := container.Cmd{
		Run: "ls " + readTarget + " > /dev/null && echo readable",
	}
	_, resultCh, callErr := b.Exec(ctx, h, cmd)
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	result := <-resultCh
	t.Logf("read-config-ok: exit=%d stdout=%q", result.ExitCode, result.Stdout)

	if result.ExitCode != 0 {
		t.Errorf("read of allowed path failed: exit=%d stdout=%q", result.ExitCode, result.Stdout)
	}
	if !strings.Contains(string(result.Stdout), "readable") {
		t.Errorf("stdout %q missing \"readable\"", result.Stdout)
	}
}
