package goose_test

import (
	"testing"

	"github.com/valbaudo/awf/agent/goose"
)

func TestRefAndCapabilities(t *testing.T) {
	a, err := goose.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Ref() != "block/goose" {
		t.Errorf("Ref() = %q, want %q", a.Ref(), "block/goose")
	}
	if a.Capabilities().NativeSchema {
		t.Errorf("Capabilities().NativeSchema = true, want false (layer-2 adapter)")
	}
}

func TestWithEnv_EmptyMapOK(t *testing.T) {
	if _, err := goose.New(goose.WithEnv(nil)); err != nil {
		t.Fatalf("New(WithEnv(nil)): %v", err)
	}
}
