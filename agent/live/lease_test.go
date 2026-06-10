package live_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent/live"
)

func TestLeaseAcquireConflictReleaseAndStaleRecovery(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	now := time.Unix(1_781_114_400, 0)
	req := live.LeaseRequest{
		AdapterRef:    "openai/codex-live",
		SessionKey:    "builder",
		OwnerRunID:    "run-1",
		OwnerPID:      os.Getpid(),
		OwnerNodePath: "build",
		OwnerEpoch:    2,
		LeaseID:       live.LeaseID("run-1", 2, "build", "builder"),
		TTLSeconds:    120,
	}
	lease, err := live.AcquireLease(root, req, now, nil)
	if err != nil {
		t.Fatalf("AcquireLease first owner: %v", err)
	}
	if lease.Schema != live.LeaseSchema || lease.LeaseID != req.LeaseID || lease.HeartbeatUnix != now.Unix() {
		t.Fatalf("lease = %+v, want schema %q id %q and heartbeat %d", lease, live.LeaseSchema, req.LeaseID, now.Unix())
	}
	if !strings.HasPrefix(lease.LeaseID, "sha256:") || len(lease.LeaseID) != len("sha256:")+64 {
		t.Fatalf("LeaseID = %q, want sha256 digest", lease.LeaseID)
	}

	conflict := req
	conflict.OwnerRunID = "run-2"
	conflict.LeaseID = live.LeaseID("run-2", 2, "build", "builder")
	_, err = live.AcquireLease(root, conflict, now.Add(10*time.Second), nil)
	if !errors.Is(err, live.ErrLiveLeaseConflict) {
		t.Fatalf("AcquireLease active conflict err = %v, want ErrLiveLeaseConflict", err)
	}

	if err := live.ReleaseLease(root, req.AdapterRef, req.SessionKey, req.LeaseID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if _, err := live.AcquireLease(root, conflict, now.Add(20*time.Second), nil); err != nil {
		t.Fatalf("AcquireLease after release: %v", err)
	}

	unknown := conflict
	unknown.OwnerRunID = "run-unknown"
	unknown.LeaseID = live.LeaseID("run-unknown", 2, "build", "builder")
	_, err = live.AcquireLease(root, unknown, now.Add(200*time.Second), nil)
	if !errors.Is(err, live.ErrLiveLeaseStaleOwned) {
		t.Fatalf("AcquireLease stale without run liveness oracle err = %v, want ErrLiveLeaseStaleOwned", err)
	}

	stale := conflict
	stale.OwnerRunID = "run-3"
	stale.LeaseID = live.LeaseID("run-3", 2, "build", "builder")
	_, err = live.AcquireLease(root, stale, now.Add(200*time.Second), func(runID string) bool {
		return runID == conflict.OwnerRunID
	})
	if !errors.Is(err, live.ErrLiveLeaseStaleOwned) {
		t.Fatalf("AcquireLease stale but run held err = %v, want ErrLiveLeaseStaleOwned", err)
	}
	recovered, err := live.AcquireLease(root, stale, now.Add(250*time.Second), func(string) bool { return false })
	if err != nil {
		t.Fatalf("AcquireLease stale recovered: %v", err)
	}
	if recovered.LeaseID != stale.LeaseID || recovered.OwnerRunID != "run-3" {
		t.Fatalf("recovered lease = %+v, want stale owner replaced", recovered)
	}
}

func TestAcquireLeaseRejectsNonPositiveTTL(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	_, err = live.AcquireLease(root, live.LeaseRequest{
		AdapterRef:    "openai/codex-live",
		SessionKey:    "builder",
		OwnerRunID:    "run-1",
		OwnerPID:      os.Getpid(),
		OwnerNodePath: "build",
		OwnerEpoch:    1,
		LeaseID:       live.LeaseID("run-1", 1, "build", "builder"),
		TTLSeconds:    0,
	}, time.Unix(1, 0), nil)
	if err == nil {
		t.Fatal("AcquireLease with TTLSeconds=0 succeeded, want error")
	}
}
