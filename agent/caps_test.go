package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/agent/claudesession"
	"github.com/valbaudo/awf/agent/codexlive"
	"github.com/valbaudo/awf/agent/droid"
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

func TestCapsSurfacesLivenessZeroValueIsNone(t *testing.T) {
	// Zero value must be LivenessNone so an adapter we have NOT measured
	// honestly declares "no live progress signal" rather than overclaiming.
	var c agent.Caps
	if c.SurfacesLiveness != agent.LivenessNone {
		t.Errorf("zero-value Caps.SurfacesLiveness = %d, want LivenessNone (0)", c.SurfacesLiveness)
	}
}

func TestCapsSurfacesLivenessMeasuredAdapterTiers(t *testing.T) {
	// codexlive forwards reasoning-summary deltas (D1) -> Coarse. It is the ONLY
	// adapter with a genuine liveness signal.
	codexLive, err := codexlive.New()
	if err != nil {
		t.Fatalf("codexlive.New: %v", err)
	}
	if got := codexLive.Capabilities().SurfacesLiveness; got != agent.LivenessCoarse {
		t.Errorf("codexlive SurfacesLiveness = %d, want LivenessCoarse", got)
	}

	// claude-code / claude-code-session emit one AgentEvent per COMPLETE stream-json
	// message (no --include-partial-messages token deltas) and go silent during tool
	// execution -> None (no signal an idle watchdog can trust).
	claudeAdapter, err := claude.New()
	if err != nil {
		t.Fatalf("claude.New: %v", err)
	}
	if got := claudeAdapter.Capabilities().SurfacesLiveness; got != agent.LivenessNone {
		t.Errorf("claude SurfacesLiveness = %d, want LivenessNone", got)
	}
	claudeSess, err := claudesession.New()
	if err != nil {
		t.Fatalf("claudesession.New: %v", err)
	}
	if got := claudeSess.Capabilities().SurfacesLiveness; got != agent.LivenessNone {
		t.Errorf("claudesession SurfacesLiveness = %d, want LivenessNone", got)
	}

	// Every unmeasured adapter keeps the zero value (None).
	droidAdapter, err := droid.New()
	if err != nil {
		t.Fatalf("droid.New: %v", err)
	}
	if got := droidAdapter.Capabilities().SurfacesLiveness; got != agent.LivenessNone {
		t.Errorf("droid SurfacesLiveness = %d, want LivenessNone", got)
	}
}
