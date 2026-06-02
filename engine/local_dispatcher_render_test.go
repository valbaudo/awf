package engine

import (
	"bytes"
	"io"
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestEventRenderer_DefaultsToTerseFallback(t *testing.T) {
	r := (&LocalDispatcher{}).eventRenderer()
	if r == nil {
		t.Fatal("eventRenderer() must never return nil")
	}
	var b bytes.Buffer
	r(&b, agent.AgentEvent{Kind: "k", Payload: []byte("p")})
	if b.String() != "[k] p\n" {
		t.Errorf("default = %q, want the terse fallback", b.String())
	}
}

func TestEventRenderer_UsesInjected(t *testing.T) {
	called := false
	d := &LocalDispatcher{RenderAgentEvent: func(io.Writer, agent.AgentEvent) { called = true }}
	d.eventRenderer()(&bytes.Buffer{}, agent.AgentEvent{})
	if !called {
		t.Error("must return the injected RenderAgentEvent")
	}
}
