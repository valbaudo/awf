package agent

import (
	"reflect"
	"testing"
)

func TestDisplayClass_ZeroValueIsOther(t *testing.T) {
	var c DisplayClass
	if c != DisplayOther {
		t.Errorf("zero DisplayClass = %d, want DisplayOther (0)", c)
	}
}

func TestAgentEvent_DisplayDefaultsToOther(t *testing.T) {
	ev := AgentEvent{Kind: "whatever", Payload: []byte("{}")}
	if ev.Display.Class != DisplayOther {
		t.Errorf("default Display.Class = %d, want DisplayOther", ev.Display.Class)
	}
}

// TestAgentEvent_StaysComparable verifies that EventDisplay contains only scalar
// fields so it does not make AgentEvent harder to compare. AgentEvent already
// carries a []byte Payload which prevents struct ==; we use reflect.DeepEqual
// to assert the semantic intent (identical values are equal) without a
// compile-time error.
func TestAgentEvent_StaysComparable(t *testing.T) {
	a := AgentEvent{Kind: "k", Display: EventDisplay{Class: DisplayToolCall, Tool: "T"}}
	b := AgentEvent{Kind: "k", Display: EventDisplay{Class: DisplayToolCall, Tool: "T"}}
	if !reflect.DeepEqual(a, b) {
		t.Error("equal AgentEvents must compare equal")
	}
	// EventDisplay itself (no []byte) must be == comparable.
	da := EventDisplay{Class: DisplayToolCall, Tool: "T"}
	db := EventDisplay{Class: DisplayToolCall, Tool: "T"}
	if da != db {
		t.Error("equal EventDisplays must compare ==")
	}
}
