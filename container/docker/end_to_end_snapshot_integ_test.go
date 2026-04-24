//go:build integ

package docker

import (
	"context"
	"testing"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/backendtest"
)

// TestE2E_RunSnapshotContractAgainstRealDocker invokes the parameterized
// SnapshotFSCoW contract test against the Docker backend. Uses the slice-4.4
// backendWithSleepInfinity adapter which follows the slice-4.1
// backendWithDefaultImage augment-then-delegate pattern: adds Cmd:
// sleep-infinity to the ContainerSpec (slice-4.4 Design Q9), then delegates
// to the production Backend.Create. NO Backend.Create bypass — the
// contract exercises the full image-mode Create path.
//
// Slice-4.4 Design Q8: the injected Cmd is captured into the SnapshotRef
// via ContainerInspect; Restore re-creates with the same Cmd → restored
// container also runs sleep-infinity → stays alive for CopyToContainer +
// per-delete Exec.
//
// Production snapshot:workspace images would supply their own long-running
// ENTRYPOINT in the image; the adapter is a test-only fixture quirk.
func TestE2E_RunSnapshotContractAgainstRealDocker(t *testing.T) {
	cli, b := newTestBackend(t, "e2e-snapshot-contract")
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}
	contract := &backendWithSleepInfinity{Backend: b}
	backendtest.RunSnapshotContract(t, contract, alpineDigest, "lab")
}

// backendWithSleepInfinity wraps Backend to inject Cmd: sleep-infinity for
// image-mode Create calls when no Cmd is already set. Follows the slice-4.1
// backendWithDefaultImage augment-then-delegate pattern — Backend.Create is
// CALLED (not bypassed), so the contract test exercises the full production
// image-mode path (resource limits, registration in handles map, etc.).
//
// Compose-mode passes through unchanged. spec.Cmd already-set is respected
// (production caller's choice wins).
type backendWithSleepInfinity struct {
	*Backend
}

func (a *backendWithSleepInfinity) Create(ctx context.Context, spec cont.ContainerSpec) (cont.Handle, error) {
	if spec.Image != "" && len(spec.Cmd) == 0 {
		spec.Cmd = []string{"sleep", "infinity"}
	}
	return a.Backend.Create(ctx, spec)
}
