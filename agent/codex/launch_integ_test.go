//go:build integ && live

package codex_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

func skipUnlessCodexLive(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}
	if os.Getenv("AWF_CODEX_LIVE") == "" {
		t.Skip("AWF_CODEX_LIVE not set; skipping real-codex smoke")
	}
}

func liveCodexEnv() map[string]string {
	out := map[string]string{}
	for _, k := range codex.DefaultEnvAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
}

func newLiveCodex(t *testing.T) (*codex.Adapter, container.Handle) {
	t.Helper()
	nb, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	a, err := codex.New(codex.WithBackend(nb), codex.WithEnv(liveCodexEnv()))
	if err != nil {
		t.Fatalf("codex.New: %v", err)
	}
	h, err := nb.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = nb.Destroy(context.Background(), h) })
	return a, h
}

// TestCodexLive_ToolUse_LastWins exercises the highest-value codex-specific path
// against the REAL binary: a tool-forcing prompt under an output_schema, so the
// multiple-agent_message last-wins selection survives a real command_execution
// (codex bug #19816: the schema constrains intermediate messages too).
func TestCodexLive_ToolUse_LastWins(t *testing.T) {
	skipUnlessCodexLive(t)
	a, h := newLiveCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	schema := ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required":   []any{"file_count"},
		"properties": map[string]any{"file_count": map[string]any{"type": "integer"}},
	}
	inv := agent.AgentInvocation{
		NodePath:     "/live/tooluse",
		Uses:         codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "Run a shell command to count the files in the current directory, then answer with that integer as file_count."},
		OutputSchema: &schema,
	}
	events, outcomeCh, err := a.Launch(ctx, h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	sawCmd := false
	for ev := range events {
		if ev.Kind == "command_execution" {
			sawCmd = true
		}
	}
	oc := <-outcomeCh
	if oc.Err != nil {
		t.Fatalf("outcome err: %v", oc.Err)
	}
	if _, ok := oc.Result.Output["file_count"].(float64); !ok {
		t.Fatalf("Output missing integer file_count: %+v", oc.Result.Output)
	}
	if !sawCmd {
		t.Errorf("expected a command_execution event (tool use) — last-wins path not exercised")
	}
}

// TestCodexLive_BadModel_Permanent asserts the real turn.failed classifier: a bad
// --model yields status 400 / invalid_request_error → permanent ErrInvalidConfig.
func TestCodexLive_BadModel_Permanent(t *testing.T) {
	skipUnlessCodexLive(t)
	a, h := newLiveCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inv := agent.AgentInvocation{
		NodePath: "/live/badmodel",
		Uses:     codex.AdapterRef,
		With:     ir.RawConfig{"prompt": "hi", "model": "not-a-real-model-xyz"},
	}
	events, outcomeCh, err := a.Launch(ctx, h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range events {
	}
	oc := <-outcomeCh
	var bad *agent.ErrInvalidConfig
	if !errors.As(oc.Err, &bad) {
		t.Fatalf("bad-model outcome = %v, want *agent.ErrInvalidConfig (permanent)", oc.Err)
	}
}
