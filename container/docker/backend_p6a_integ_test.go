//go:build integ

package docker

import (
	"context"
	"strings"
	"testing"

	cont "github.com/valbaudo/awf/container"
)

// TestP6a_CapabilitiesAdvertisesRuntimeImage locks the capability flip: the
// docker backend now honors a map's runtime-resolved image:, so the CLI guard
// (cli/runtimeimageguard.go) must let such workflows through on docker.
func TestP6a_CapabilitiesAdvertisesRuntimeImage(t *testing.T) {
	_, b := newTestBackend(t, "p6a-caps")
	if !b.Capabilities().RuntimeImage {
		t.Error("Caps.RuntimeImage = false, want true (docker honors runtime-resolved map images)")
	}
}

// TestP6a_CreatePullsAndCapturesDigest is the core proof: with PullIfAbsent set
// and a digest-pinned ref, Create pulls the image itself (no pre-provisioning),
// boots it, and reports the booted content digest on Handle.ResolvedImageDigest
// — the durable record the engine writes onto the map.item commit.
func TestP6a_CreatePullsAndCapturesDigest(t *testing.T) {
	_, b := newTestBackend(t, "p6a-pull")
	ctx := context.Background()

	// No pre-pull: PullIfAbsent makes Create fetch the digest. sleep infinity
	// keeps the container up (no healthcheck → waitReady returns immediately).
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:         "lab",
		Image:        alpineDigest,
		PullIfAbsent: true,
		Cmd:          []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create(PullIfAbsent, %s): %v", alpineDigest, err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	if h.ResolvedImageDigest != alpineDigest {
		t.Errorf("Handle.ResolvedImageDigest = %q, want %q", h.ResolvedImageDigest, alpineDigest)
	}
}

// TestP6a_CreateRejectsMutableTag guards the reproducibility gate: a
// runtime-resolved image MUST be digest-pinned. A mutable tag is rejected
// before any pull/create, so the booted bytes can never drift across a
// run/resume (the pin-before-run invariant, applied to the runtime-image case).
func TestP6a_CreateRejectsMutableTag(t *testing.T) {
	_, b := newTestBackend(t, "p6a-tag")
	ctx := context.Background()

	_, err := b.Create(ctx, cont.ContainerSpec{
		Name:         "lab",
		Image:        "alpine:3.20", // a tag, not name@sha256:…
		PullIfAbsent: true,
	})
	if err == nil {
		t.Fatal("Create(PullIfAbsent, mutable tag): err = nil, want a digest-pin rejection")
	}
	if !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("error = %v, want it to mention the digest-pin requirement", err)
	}
}
