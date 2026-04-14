package conformance

import "testing"

// testReplay is Bucket 2 — exact committed-prefix replay. Task 5 lands it.
func testReplay(t *testing.T, factory BackendFactory) {
	t.Helper()
	_ = factory
	_ = fiveStepSeqWorkflow // used by Task 5
	t.Skip("Task 5 of slice 2.6 lands this bucket")
}
