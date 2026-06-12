package state

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

// Pin: state blob refs and ir workflow digests MUST share the scheme prefix.
// A future awf-d2 bump in one place must not silently desync the other.
func TestBlobRefPrefixMatchesDigestScheme(t *testing.T) {
	if blobRefPrefix != ir.DigestScheme {
		t.Fatalf("blobRefPrefix %q != ir.DigestScheme %q", blobRefPrefix, ir.DigestScheme)
	}
}
