package live_test

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent/live"
)

func TestLiveVersionIncludesProtocolDigest(t *testing.T) {
	got := live.FormatVersion("codex-cli/0.137.0", "sha256:abc123")
	for _, want := range []string{"codex-cli/0.137.0", "protocol-schema/sha256:abc123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatVersion = %q, want to contain %q", got, want)
		}
	}
}
