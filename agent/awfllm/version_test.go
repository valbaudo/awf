package awfllm_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/container"
)

func TestVersion_StaticConstant(t *testing.T) {
	a, _ := awfllm.New()
	// Ignores ctx + handle (no binary; the model is pinned via the definition
	// digest). Must be network-free and never error.
	v, err := a.Version(context.Background(), container.Handle{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "awf-llm/1" {
		t.Errorf("Version = %q, want %q", v, "awf-llm/1")
	}
}
