package docker

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

// Pin: the snapshot-ref blob scheme MUST match ir.DigestScheme.
func TestSnapshotBlobSchemeMatchesDigestScheme(t *testing.T) {
	if snapshotBlobScheme != ir.DigestScheme {
		t.Fatalf("snapshotBlobScheme %q != ir.DigestScheme %q", snapshotBlobScheme, ir.DigestScheme)
	}
}
