package codex_test

import (
	"testing"

	"github.com/valbaudo/awf/agent/codex"
)

func TestRefAndCapabilities(t *testing.T) {
	a, err := codex.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Ref() != "openai/codex" {
		t.Errorf("Ref() = %q, want %q", a.Ref(), "openai/codex")
	}
	if !a.Capabilities().NativeSchema {
		t.Errorf("Capabilities().NativeSchema = false, want true (native --output-schema adapter)")
	}
}

func TestWithEnv_EmptyMapOK(t *testing.T) {
	if _, err := codex.New(codex.WithEnv(nil)); err != nil {
		t.Fatalf("New(WithEnv(nil)): %v", err)
	}
}
