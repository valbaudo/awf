package agent_test

import (
	"encoding/json"
	"strings"
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

func TestCapsPersistentSessionZeroValue(t *testing.T) {
	var c agent.Caps
	if c.PersistentSession {
		t.Error("zero-value Caps.PersistentSession = true, want false")
	}
}

func TestCapsPersistentSessionLiveRendering(t *testing.T) {
	c := agent.Caps{PersistentSession: true}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"persistent_session":true`) {
		t.Fatalf("Caps JSON %q missing persistent_session=true", b)
	}
}
