package container

import (
	"context"
	"testing"
)

func TestFakeCreateResolvesProgrammedImageDigest(t *testing.T) {
	f := NewFake()
	f.ProgramImageDigest("registry.example.com/app:1.2.3", "registry.example.com/app@sha256:aaa")

	h, err := f.Create(context.Background(), ContainerSpec{Name: "lab", Image: "registry.example.com/app:1.2.3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ResolvedImageDigest != "registry.example.com/app@sha256:aaa" {
		t.Errorf("ResolvedImageDigest = %q, want the programmed digest", h.ResolvedImageDigest)
	}
}

func TestFakeFailCreateForImage(t *testing.T) {
	f := NewFake()
	f.FailCreateForImage("registry.example.com/app:gone")

	if _, err := f.Create(context.Background(), ContainerSpec{Name: "lab", Image: "registry.example.com/app:gone"}); err == nil {
		t.Fatal("Create against a fail-programmed image: err = nil, want failure")
	}
	h, err := f.Create(context.Background(), ContainerSpec{Name: "lab", Image: "registry.example.com/app:ok"})
	if err != nil {
		t.Errorf("Create against a normal image: %v", err)
	}
	if h.ResolvedImageDigest != "" {
		t.Errorf("unprogrammed image: ResolvedImageDigest = %q, want empty", h.ResolvedImageDigest)
	}
}

func TestFakeAdvertisesRuntimeImageCapability(t *testing.T) {
	if !NewFake().Capabilities().RuntimeImage {
		t.Error("fake Caps.RuntimeImage = false, want true (fake resolves runtime images)")
	}
}
