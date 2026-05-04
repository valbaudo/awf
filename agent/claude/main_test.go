package claude

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak.VerifyTestMain so any goroutine leaked by Launch's
// streaming consumer (or future code) fails the test run immediately. The
// pattern was established in slice 4.1 for container/docker; runtime-design.md
// §15 mandates it for any new goroutine-introducing package.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
