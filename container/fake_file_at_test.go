package container

import (
	"context"
	"errors"
	"testing"
)

func TestFakeReadWriteFileAtRoundTrip(t *testing.T) {
	f := NewFake()
	h, err := f.Create(context.Background(), ContainerSpec{Name: "c"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []byte(`{"transcript":"line1"}`)
	if err := f.WriteFileAt(context.Background(), h, "/home/agent/.claude/projects/p/s.jsonl", want); err != nil {
		t.Fatalf("WriteFileAt: %v", err)
	}
	got, err := f.ReadFileAt(context.Background(), h, "/home/agent/.claude/projects/p/s.jsonl")
	if err != nil {
		t.Fatalf("ReadFileAt: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFileAt = %q, want %q", got, want)
	}
	if len(f.WriteFileAtCalls) != 1 || f.WriteFileAtCalls[0].Path != "/home/agent/.claude/projects/p/s.jsonl" {
		t.Errorf("WriteFileAtCalls = %+v, want one record for the path", f.WriteFileAtCalls)
	}
}

func TestFakeReadFileAtMissing(t *testing.T) {
	f := NewFake()
	h, _ := f.Create(context.Background(), ContainerSpec{Name: "c"})
	if _, err := f.ReadFileAt(context.Background(), h, "/nope"); err == nil {
		t.Fatal("ReadFileAt of a missing path: want error, got nil")
	}
}

func TestFakeFileAtCtxCancel(t *testing.T) {
	f := NewFake()
	h, _ := f.Create(context.Background(), ContainerSpec{Name: "c"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.WriteFileAt(ctx, h, "/p", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteFileAt cancelled: err = %v, want context.Canceled", err)
	}
}
