package docker

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// VerifyTestMain runs the suite, then checks for leaked goroutines.
	// Slice 4.1 ships stub-only tests in container/docker/ at this commit
	// (the integ tests with real Docker streams land in Task 7 under
	// //go:build integ). The discipline is in place BEFORE Task 7 adds
	// Bucket 9a's ImagePull / ContainerInspect / ContainerRemove paths.
	goleak.VerifyTestMain(m)
}
