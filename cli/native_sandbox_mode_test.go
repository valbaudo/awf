package cli

// F30: unit coverage for printNativeSandboxMode — the run-start stderr line
// surfacing the native backend's resolved sandbox mode. White-box (package
// cli, not cli_test) because printNativeSandboxMode is unexported.
//
// These tests call printNativeSandboxMode directly rather than driving a full
// `awf run` — native.Backend.SandboxMode() is a pure getter (no exec), so
// constructing one via native.New and calling the helper exercises the real
// wiring without spawning any host process. Per this task's brief: any CLI
// test that drives a REAL --backend native run belongs in a //go:build integ
// file (see cli/run_backend_integ_test.go, which separately asserts the same
// stderr line during an actual native run-to-completion).

import (
	"bytes"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestPrintNativeSandboxMode_NativeBackendPrintsResolvedMode asserts that a
// real *native.Backend causes the run-start sandbox-mode line to appear on
// stderr, carrying whatever mode the backend resolved (SandboxMode()).
func TestPrintNativeSandboxMode_NativeBackendPrintsResolvedMode(t *testing.T) {
	nat, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}

	var stderr bytes.Buffer
	printNativeSandboxMode(nat, &stderr)

	want := "awf run: native sandbox: " + nat.SandboxMode() + "\n"
	if got := stderr.String(); got != want {
		t.Errorf("printNativeSandboxMode stderr = %q, want %q", got, want)
	}
}

// TestPrintNativeSandboxMode_NonNativeBackendIsNoOp asserts that a non-native
// backend (the fake, standing in for docker/fake in production) produces no
// output — the sandbox-mode line is native-specific.
func TestPrintNativeSandboxMode_NonNativeBackendIsNoOp(t *testing.T) {
	var stderr bytes.Buffer
	printNativeSandboxMode(container.NewFake(), &stderr)

	if got := stderr.String(); got != "" {
		t.Errorf("printNativeSandboxMode with fake backend wrote %q, want empty", got)
	}
}
