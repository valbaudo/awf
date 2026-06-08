package container

import (
	"context"
	"errors"
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

func TestFakeAdvertisesRuntimeComposeCapability(t *testing.T) {
	if !NewFake().Capabilities().RuntimeCompose {
		t.Error("fake Caps.RuntimeCompose = false, want true (fake promotes runtime compose specs)")
	}
}

// The fake's two Create-failure hooks must return DISTINGUISHABLE error types —
// the engine routes them differently inside a map: FailCreateForImage models a
// tolerated availability failure (*ImageUnavailableError → item_failed +
// image_unavailable), FailCreateConfigForImage models a deterministic definition
// fault (a PLAIN error → permanent_failure for the whole map). Guard the seam the
// conformance map tests depend on.
func TestFakeCreateFailureErrorTypes(t *testing.T) {
	f := NewFake()
	f.FailCreateForImage("img:gone")
	f.FailCreateConfigForImage("img:badcfg")

	_, err := f.Create(context.Background(), ContainerSpec{Name: "lab", Image: "img:gone"})
	var iu *ImageUnavailableError
	if !errors.As(err, &iu) {
		t.Errorf("FailCreateForImage Create err = %v, want *ImageUnavailableError", err)
	}

	_, err = f.Create(context.Background(), ContainerSpec{Name: "lab", Image: "img:badcfg"})
	if err == nil {
		t.Fatal("FailCreateConfigForImage Create err = nil, want non-nil")
	}
	var iu2 *ImageUnavailableError
	if errors.As(err, &iu2) {
		t.Errorf("FailCreateConfigForImage Create err = %v, want a PLAIN error (NOT *ImageUnavailableError)", err)
	}
}
