package agent_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestCaps_ContainerlessZeroValueIsFalse(t *testing.T) {
	// Zero value must be false so every existing adapter (claude/droid/goose)
	// keeps "requires a container" semantics without changing their Caps literal.
	var c agent.Caps
	if c.Containerless {
		t.Error("zero-value Caps.Containerless = true, want false")
	}
}

func TestCaps_ContainerlessSettable(t *testing.T) {
	c := agent.Caps{NativeSchema: false, Containerless: true}
	if !c.Containerless {
		t.Error("Caps{Containerless:true}.Containerless = false")
	}
}
