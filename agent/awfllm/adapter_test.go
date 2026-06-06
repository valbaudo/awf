package awfllm_test

import (
	"testing"

	"github.com/valbaudo/awf/agent/awfllm"
)

func TestRefAndCapabilities(t *testing.T) {
	a, err := awfllm.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Ref() != "awf/llm" {
		t.Errorf("Ref() = %q, want %q", a.Ref(), "awf/llm")
	}
	caps := a.Capabilities()
	if caps.NativeSchema {
		t.Error("NativeSchema = true, want false (layer-2: adapter parses message content)")
	}
	if !caps.Containerless {
		t.Error("Containerless = false, want true (direct HTTP, no container)")
	}
	if !caps.Threaded {
		t.Error("Threaded = false, want true (adapter supports engine-supplied continues: threads)")
	}
}

func TestWithEnv_EmptyOK(t *testing.T) {
	if _, err := awfllm.New(awfllm.WithEnv(nil)); err != nil {
		t.Fatalf("New(WithEnv(nil)): %v", err)
	}
}
