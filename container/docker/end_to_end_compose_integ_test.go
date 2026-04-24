//go:build integ

package docker

import (
	"context"
	"strings"
	"testing"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// TestE2E_ComposeContainerStepDispatch exercises the full
// LocalDispatcher → docker.Backend compose-mode pipeline:
//
//  1. ContainerSpecFor reads compose bytes from the composeFiles map.
//  2. docker.Backend.Create dispatches to createCompose → Up --wait.
//  3. Dispatcher.Run("container: lab") routes to the default service (web).
//  4. Dispatcher.Run("container: lab:db") routes to the db service
//     (cross-service via splitContainerRef + Handle.Service override).
//  5. Backend.Exec for each routes to the correct docker container.
//  6. Backend.Destroy tears the project down (cleanup-on-test-exit).
//
// Workflow fixture (cli/testdata/phase4/parallel-compose.yaml) is
// documentation-only — this test constructs its LoadedDefinition inline
// and references Task 6's compose-two-svc.yml fixture for the bytes.
//
// Service identification via /tmp/awf-svc-marker (written by each
// service's entrypoint in the fixture) — deterministic, independent
// of Docker's undocumented hostname defaults (H5-revised).
//
// This is the slice 4.3 analog of slice 4.2's
// TestE2E_AWFOutputContractAgainstRealDocker — same pattern (drive
// LocalDispatcher against a real docker.Backend), different scope
// (compose-mode vs image-mode-with-AWF_OUTPUT).
func TestE2E_ComposeContainerStepDispatch(t *testing.T) {
	_, b := newTestBackend(t, "e2e-compose-dispatch")
	ctx := context.Background()
	if err := pullImage(ctx, b.cli, alpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	composeBytes := loadComposeFixture(t, "cli/testdata/phase4/compose-two-svc.yml")

	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"lab": {Compose: "compose-two-svc.yml", Service: "web"},
		},
	}
	composeFiles := map[string][]byte{
		"compose-two-svc.yml": composeBytes,
	}

	spec := engine.ContainerSpecFor(wf, composeFiles, "lab")
	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Backend.Create compose: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	d := &engine.LocalDispatcher{
		Backend:      b,
		Handles:      map[string]cont.Handle{"lab": h},
		ComposeFiles: composeFiles,
	}

	// Step 1: container: lab (default service web).
	// Each service writes /tmp/awf-svc-marker with its own name — the
	// deterministic identifier (no hostname dependence).
	webIntent := engine.NodeIntent{
		Path:           "echo_web",
		Node:           &ir.CodeStep{ID: "echo_web", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{Command: "cat /tmp/awf-svc-marker"},
	}
	webResult, webCh, err := d.Run(ctx, webIntent)
	if err != nil {
		t.Fatalf("Dispatcher.Run web: %v", err)
	}
	if webResult.Outcome != engine.OutcomeOK {
		t.Errorf("web Outcome = %v, want ok (err=%v)", webResult.Outcome, webResult.Err)
	}
	if got := strings.TrimSpace(string(webResult.Stdout)); got != "web" {
		t.Errorf("web marker = %q, want \"web\"", got)
	}
	for range webCh {
	}

	// Step 2: container: lab:db (cross-service).
	dbIntent := engine.NodeIntent{
		Path:           "echo_db",
		Node:           &ir.CodeStep{ID: "echo_db", Container: "lab:db"},
		ResolvedInputs: engine.ResolvedInputs{Command: "cat /tmp/awf-svc-marker"},
	}
	dbResult, dbCh, err := d.Run(ctx, dbIntent)
	if err != nil {
		t.Fatalf("Dispatcher.Run db: %v", err)
	}
	if dbResult.Outcome != engine.OutcomeOK {
		t.Errorf("db Outcome = %v, want ok (err=%v)", dbResult.Outcome, dbResult.Err)
	}
	if got := strings.TrimSpace(string(dbResult.Stdout)); got != "db" {
		t.Errorf("db marker = %q, want \"db\"", got)
	}
	for range dbCh {
	}
}
