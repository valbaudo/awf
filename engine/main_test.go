package engine_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak.VerifyTestMain so any goroutine leaked by future
// engine code fails the test run immediately. Task 6 introduced the
// drain-to-slice pattern for LocalDispatcher.runCode (no goroutine, no
// leak risk) — but future engine refactors that spawn goroutines (e.g.,
// Task 7c's runAgent concurrent drain) need this safety net in place.
// Per runtime-design.md §15, every package that may spawn goroutines
// gets goleak coverage before the first goroutine ships.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
