//go:build integ

package conformance

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/container"
)

// Bucket 10 compose fixture paths (repo-relative). loadComposeFixture
// (docker_suite_test.go) reads them at test time via filepath.Join("..", path).
// Single source of truth at cli/testdata/phase4/*.yml — no duplication.
const (
	composeBasicFixture     = "cli/testdata/phase4/compose-basic.yml"
	composeTwoSvcFixture    = "cli/testdata/phase4/compose-two-svc.yml"
	composeSlowReadyFixture = "cli/testdata/phase4/compose-slow-ready.yml"
)

// testBucket10 runs the Phase 4 design §G "Bucket 10 — Docker (compose-mode)"
// inventory. Migrated from container/docker/compose_integ_test.go
// (slice 4.3) in slice 4.6.
func testBucket10(t *testing.T, factory DockerBackendFactory) {
	t.Helper()
	t.Run("up_exec_down", func(t *testing.T) { testBucket10UpExecDown(t, factory) })
	t.Run("cross_service_exec", func(t *testing.T) { testBucket10CrossServiceExec(t, factory) })
	t.Run("up_wait_honors_healthcheck", func(t *testing.T) { testBucket10UpWaitHonorsHealthcheck(t, factory) })
}

// testBucket10UpExecDown migrates TestBucket10a_ComposeBasicUpExecDown.
// Exec into the default service ("web") echoes "hello"; exit 0.
//
// The original asserted h.ID == composeProjectName(b.runID, "lab") —
// that's Docker-impl-specific naming (composeProjectName is package-private
// to container/docker; verified against container/docker/naming.go to
// return "awf-<runID>-lab"). The conformance bucket correctly drops that
// assertion — Handle.ID format is impl-private per the Backend interface
// doc-comment (slice 4.3); the backendtest.RunBasicContract caller in
// container/docker keeps any naming assertion it needs. The contract
// this bucket asserts is Handle.Service routing + Exec stdout/exit_code.
func testBucket10UpExecDown(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket10a-basic")
	composeBytes := loadComposeFixture(t, composeBasicFixture)
	h := env.NewComposeHandle(t, "lab", composeBytes, composeBasicFixture, "web")
	if h.Service != "web" {
		t.Errorf("Handle.Service = %q, want \"web\"", h.Service)
	}

	ch, resultCh, err := env.Backend.Exec(context.Background(), h, container.Cmd{Run: "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	result := <-resultCh
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("hello")) {
		t.Errorf("Stdout = %q, want to contain hello", result.Stdout)
	}
}

// testBucket10CrossServiceExec migrates TestBucket10b_ComposeCrossServiceExec.
// One Backend.Create call brings up the project; cross-service Exec uses
// the Handle.Service field — copy the handle, mutate .Service, Exec
// against the copy. Matches the original test's pattern exactly (verified
// against container/docker/compose_integ_test.go:80-112).
//
// The compose-two-svc fixture writes a unique marker file per service at
// startup; Exec reads the marker to deterministically identify which
// container the Exec landed in (independent of Docker's undocumented
// hostname defaults).
func testBucket10CrossServiceExec(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket10b-cross")
	composeBytes := loadComposeFixture(t, composeTwoSvcFixture)
	h := env.NewComposeHandle(t, "lab", composeBytes, composeTwoSvcFixture, "web")
	ctx := context.Background()

	// Default service: web.
	ch, resultCh, err := env.Backend.Exec(ctx, h, container.Cmd{Run: "cat /tmp/awf-svc-marker"})
	if err != nil {
		t.Fatalf("Exec web: %v", err)
	}
	for range ch {
	}
	result := <-resultCh
	if got := strings.TrimSpace(string(result.Stdout)); got != "web" {
		t.Errorf("default-service marker = %q, want \"web\"", got)
	}

	// Cross-service exec: same project, different service.
	crossH := h
	crossH.Service = "db"
	ch, resultCh, err = env.Backend.Exec(ctx, crossH, container.Cmd{Run: "cat /tmp/awf-svc-marker"})
	if err != nil {
		t.Fatalf("Exec db (cross-service): %v", err)
	}
	for range ch {
	}
	result = <-resultCh
	if got := strings.TrimSpace(string(result.Stdout)); got != "db" {
		t.Errorf("cross-service marker = %q, want \"db\"", got)
	}
}

// testBucket10UpWaitHonorsHealthcheck migrates
// TestBucket10c_ComposeUpWaitHonorsHealthcheck. Backend.Create blocks
// until the healthcheck reports healthy (~2s, since the entrypoint touches
// /tmp/ready after a sleep 2). Post-Create Exec succeeds first-try with
// no race against readiness.
func testBucket10UpWaitHonorsHealthcheck(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket10c-wait")
	composeBytes := loadComposeFixture(t, composeSlowReadyFixture)
	start := time.Now()
	h := env.NewComposeHandle(t, "lab", composeBytes, composeSlowReadyFixture, "slow")
	elapsed := time.Since(start)

	if elapsed < 1500*time.Millisecond {
		t.Errorf("Create returned in %v, want >=1.5s (healthcheck-gated). up --wait may have skipped healthcheck gating.", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Errorf("Create elapsed = %v, want <30s (healthcheck should succeed within ~2-3s)", elapsed)
	}

	// Post-Create Exec succeeds first-try.
	ch, resultCh, err := env.Backend.Exec(context.Background(), h, container.Cmd{Run: "test -f /tmp/ready"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	result := <-resultCh
	if result.ExitCode != 0 {
		t.Errorf("test -f /tmp/ready exit = %d, want 0 (service should be healthy)", result.ExitCode)
	}
}
