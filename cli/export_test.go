package cli

import (
	"context"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

// Test-only re-exports. Lets cli_test exercise package-private helpers
// without making them part of the cli package's public API. Compiled
// only with _test.go files; not present in production builds. Standard
// Go stdlib pattern (see encoding/json/export_test.go,
// crypto/tls/export_test.go).
//
// No `type Event = state.Event` alias here — tests construct
// `[]state.Event{...}` directly via the state import (per slice-4.5
// plan §Major #3).

func NewBackendForTest(ctx context.Context, kind, runID string, blobs state.Blobs) (container.Backend, func(), error) {
	return newBackend(ctx, kind, runID, blobs)
}

func ReadBackendKindFromLogForTest(events []state.Event) (string, error) {
	return readBackendKindFromLog(events)
}
