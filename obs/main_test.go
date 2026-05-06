package obs

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain guards against goroutine leaks (runtime-design §15). obs's own code
// spawns no goroutines; this is PRE-EMPTIVE for future otlptracehttp tests.
// RULE for obs tests: every test that builds a TracerProvider must
// `defer tp.Shutdown(ctx)` (flushes SimpleSpanProcessor + releases exporter
// resources); a test building a real OTLP transport must also
// `defer transport.CloseIdleConnections()`. Fix leaks at the SOURCE — do NOT
// add goleak.IgnoreTopFunction("net/http…") except as a documented last resort
// (matches cli/backend.go's close-at-source stance).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
