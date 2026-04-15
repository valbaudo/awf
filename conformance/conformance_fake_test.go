package conformance

import (
	"testing"

	"github.com/valbaudo/awf/container"
)

// TestConformanceFakeBackend is the Phase-2 entry point. Phase 4 will add
// TestConformanceDockerBackend in a build-tag-gated _test.go file with
// docker.NewFactory.
func TestConformanceFakeBackend(t *testing.T) {
	RunSuite(t, func() container.Backend {
		return container.NewFake()
	})
}
