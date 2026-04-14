package conformance

import "testing"

// testAtomic is Bucket 3 — atomic commit. Task 6 lands it.
func testAtomic(t *testing.T, factory BackendFactory) {
	t.Helper()
	_ = factory
	t.Skip("Task 6 of slice 2.6 lands this bucket")
}
