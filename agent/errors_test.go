package agent_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestErrAdapterAlreadyRegistered_AsAndMessage(t *testing.T) {
	err := &agent.ErrAdapterAlreadyRegistered{Ref: "anthropic/claude-code"}
	want := `agent: adapter "anthropic/claude-code" already registered`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

	wrapped := errors.Join(errors.New("ctx"), err)
	var target *agent.ErrAdapterAlreadyRegistered
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As did not unwrap to *ErrAdapterAlreadyRegistered")
	}
	if target.Ref != "anthropic/claude-code" {
		t.Errorf("unwrapped Ref = %q, want %q", target.Ref, "anthropic/claude-code")
	}
}

func TestErrAdapterNotFound_AsAndMessage(t *testing.T) {
	err := &agent.ErrAdapterNotFound{Ref: "x"}
	want := `agent: no adapter registered for "x"`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestErrInvalidConfig_AsAndMessage(t *testing.T) {
	err := &agent.ErrInvalidConfig{Ref: "x", Key: "prompt", Reason: "must be string"}
	want := `agent: adapter "x" rejected config key "prompt": must be string`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestErrUnparseableOutput_AsAndMessage(t *testing.T) {
	err := &agent.ErrUnparseableOutput{NodePath: "graph[0]"}
	want := `agent: unparseable output at node "graph[0]"`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestErrAgentLaunch_AsAndMessage(t *testing.T) {
	cause := errors.New("docker exec: i/o timeout")
	err := &agent.ErrAgentLaunch{Cause: cause}
	want := `agent: launch failed: docker exec: i/o timeout`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is did not match Cause via Unwrap")
	}
}
