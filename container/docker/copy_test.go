//go:build integ

package docker

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/container/backendtest"
)

// TestDockerCopyToRoundTrip exercises the real Docker CopyTo impl (tar →
// CopyToContainer at "/") against a live daemon. RunCopyToContract uses a NESTED
// absolute dst ("/work/sub/in.txt"), so this test verifies moby's go-archive
// auto-creation of the entry's ancestor directories (createImpliedDirectories) —
// the fake's flat map can't catch a dir bug, so this is the only place it's
// proven. RunCopyToContract Creates with no Image; the Docker backend needs one,
// so we wrap with backendWithDefaultImage (same as TestBucket9a_BackendtestContract).
func TestDockerCopyToRoundTrip(t *testing.T) {
	cli, b := newTestBackend(t, "copyto-roundtrip")

	if err := pullImage(context.Background(), cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	contract := &backendWithDefaultImage{Backend: b, defaultImage: alpineDigest}
	backendtest.RunCopyToContract(t, contract)
}
