//go:build integ

package docker

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"

	cont "github.com/valbaudo/awf/container"
)

// TestF27_CreatePullsStaticImageOnColdCache is the F27 behavior test: on the
// STATIC image-mode path (no PullIfAbsent — that's the separate P6a
// runtime-resolved-image path, already covered by
// TestP6a_CreatePullsAndCapturesDigest in backend_p6a_integ_test.go), Create
// must self-heal a cold cache by pulling the digest-pinned image and retrying
// ContainerCreate exactly once, instead of propagating docker's "no such
// image" not-found error.
//
// This is integ-only by necessity, not preference: b.cli is a concrete
// *client.Client (backend.go:71), not an interface, so the not-found retry
// cannot be exercised by stubbing a fake client in a unit test — it requires
// a real daemon returning a real not-found error from ContainerCreate. It
// COMPILES here (`go build -tags integ ./container/docker/`) but this box has
// no docker daemon; running it live is a tracked cve-runner follow-up.
//
// Note: this test mutates shared docker-daemon state (it evicts alpineDigest
// from the local image cache) and assumes it runs sequentially with the rest
// of this package's integ tests — true today since no test here calls
// t.Parallel().
func TestF27_CreatePullsStaticImageOnColdCache(t *testing.T) {
	cli, b := newTestBackend(t, "f27-cold-cache")
	ctx := context.Background()

	// Force a cold cache: evict the pinned image if resident (e.g. left over
	// from an earlier integ test in this run). A "no such image" error here
	// just means it's already absent — that's fine, not a setup failure.
	if _, err := cli.ImageRemove(ctx, alpineDigest, image.RemoveOptions{Force: true, PruneChildren: true}); err != nil {
		t.Logf("ImageRemove(%s) pre-test eviction: %v (ok if already absent)", alpineDigest, err)
	}
	if _, err := cli.ImageInspect(ctx, alpineDigest); err == nil {
		t.Fatalf("setup: %s is still in the local image cache after eviction; cannot exercise the cold-cache path", alpineDigest)
	}

	since := time.Now().UTC().Format(time.RFC3339Nano)

	// The core assertion: Create's static path (Image set, PullIfAbsent
	// false) must succeed even though the image is not cached — it should
	// detect ContainerCreate's not-found error, pull by digest, and retry
	// ContainerCreate once. Before F27 this returned an error (ContainerCreate:
	// No such image) instead of self-healing.
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "cold-cache",
		Image: alpineDigest,
		Cmd:   []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create(%s) on a cold cache: %v, want Create to self-pull and succeed", alpineDigest, err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	until := time.Now().UTC().Format(time.RFC3339Nano)

	// The image must now be resident — direct evidence Create actually pulled
	// it (as opposed to, say, the daemon silently resolving it some other way).
	if _, err := cli.ImageInspect(ctx, alpineDigest); err != nil {
		t.Errorf("ImageInspect(%s) after Create: %v, want it cached (Create should have pulled it)", alpineDigest, err)
	}

	// Replay the daemon's own event log for the Create() window to confirm
	// EXACTLY one pull happened — proof of "retry once", not a pull loop.
	// Since/Until bound a historical (already-elapsed) range, so this is a
	// synchronous read, not a live race against Create's goroutines.
	msgs, errs := cli.Events(ctx, dockerevents.ListOptions{
		Since: since,
		Until: until,
		Filters: filters.NewArgs(
			filters.Arg("type", string(dockerevents.ImageEventType)),
			filters.Arg("event", string(dockerevents.ActionPull)),
		),
	})
	pullCount := 0
eventLoop:
	for {
		select {
		case <-msgs:
			pullCount++
		case err := <-errs:
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("Events stream: %v", err)
			}
			break eventLoop
		}
	}
	if pullCount != 1 {
		t.Errorf("image pull events for %s during Create = %d, want exactly 1 (Create must pull once and retry ContainerCreate once — not loop)", alpineDigest, pullCount)
	}
}
