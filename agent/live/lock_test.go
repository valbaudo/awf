package live

import (
	"os"
	"testing"
	"time"
)

func TestSessionLockSerializesLeaseCriticalSection(t *testing.T) {
	root, err := OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withSessionLock(root, "openai/codex-live", "builder", func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	acquired := make(chan error, 1)
	go func() {
		_, err := AcquireLease(root, LeaseRequest{
			AdapterRef:    "openai/codex-live",
			SessionKey:    "builder",
			OwnerRunID:    "run-1",
			OwnerPID:      os.Getpid(),
			OwnerNodePath: "build",
			OwnerEpoch:    1,
			LeaseID:       LeaseID("run-1", 1, "build", "builder"),
			TTLSeconds:    120,
		}, time.Unix(1, 0), nil)
		acquired <- err
	}()

	select {
	case err := <-acquired:
		t.Fatalf("AcquireLease returned while session lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("withSessionLock: %v", err)
	}
	if err := <-acquired; err != nil {
		t.Fatalf("AcquireLease after lock release: %v", err)
	}
}
