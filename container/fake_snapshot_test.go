package container

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/state"
)

func TestFakeSnapshotRestoreRoundTripViaBlobs(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	ctx := context.Background()

	f1 := NewFake().WithBlobs(blobs)
	if f1.Capabilities().Snapshot != SnapshotFSCoW {
		t.Fatalf("Capabilities with blobs = %v, want fs-cow", f1.Capabilities().Snapshot)
	}
	h1, err := f1.Create(ctx, ContainerSpec{Name: "ws", Snapshot: "workspace"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f1.WriteFile(h1, "/work/state", []byte("v1")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ref, err := f1.Snapshot(ctx, h1)
	if err != nil || ref == "" {
		t.Fatalf("Snapshot: ref=%q err=%v", ref, err)
	}

	// Fresh fake (simulates resume): SAME blobs, Restore from the ref.
	f2 := NewFake().WithBlobs(blobs)
	h2, err := f2.Restore(ctx, ref, "ws")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := f2.RestoreCalls; len(got) != 1 || got[0].Name != "ws" || got[0].Ref != ref {
		t.Errorf("RestoreCalls = %+v, want one {ws, %q}", got, ref)
	}
	got, err := f2.CaptureFiles(ctx, h2, []string{"/work/state"})
	if err != nil || len(got) != 1 || string(got[0].Content) != "v1" {
		t.Fatalf("restored /work/state = %v err=%v, want v1", got, err)
	}
}

func TestFakeSnapshotWithoutBlobsUnsupported(t *testing.T) {
	f := NewFake() // no WithBlobs
	if f.Capabilities().Snapshot != SnapshotNone {
		t.Fatalf("Capabilities without blobs = %v, want none", f.Capabilities().Snapshot)
	}
	if _, err := f.Snapshot(context.Background(), Handle{}); err != ErrUnsupported {
		t.Fatalf("Snapshot without blobs err = %v, want ErrUnsupported", err)
	}
}
