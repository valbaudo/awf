package claude_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func TestLaunch_HappyPath_StructuredOutput(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"system","subtype":"init","session_id":"s1","claude_code_version":"2.1.152"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer is 42"}]}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"42","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"structured_output":{"answer":42}}
`)
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath:     "graph[0]",
		Uses:         claude.AdapterRef,
		With:         ir.RawConfig{"prompt": "what is the answer"},
		OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []string{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "integer"}}},
	}
	// γ contract: drain events first, then read AgentOutcome.
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var events []agent.AgentEvent
	for ev := range eventCh {
		events = append(events, ev)
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if len(events) < 3 {
		t.Errorf("events len = %d, want >=3 (system + text + result)", len(events))
	}
	if outcome.Result.Metrics.Cost.Total != 0.001 {
		t.Errorf("Cost.Total = %v", outcome.Result.Metrics.Cost.Total)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 42 {
		t.Errorf("Output[answer] = %v (%T)", outcome.Result.Output["answer"], outcome.Result.Output["answer"])
	}
}

func TestLaunch_ErrMaxStructuredOutputRetries(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"duration_ms":5000,"num_turns":3}
`)
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With:     ir.RawConfig{"prompt": "x"},
	}
	// γ contract: in-flight failure surfaces via outcome.Err, not the pre-launch err return.
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	for range eventCh {
	}
	outcome := <-outcomeCh
	var unparseable *agent.ErrUnparseableOutput
	if !errors.As(outcome.Err, &unparseable) {
		t.Fatalf("outcome.Err = %v; want *agent.ErrUnparseableOutput", outcome.Err)
	}
	if unparseable.NodePath != "graph[0]" {
		t.Errorf("NodePath = %q", unparseable.NodePath)
	}
}

// TestLaunch_AuthFailure_Permanent verifies a Claude Code auth failure
// (subtype:success + is_error:true + "Not logged in") classifies PERMANENT —
// the outcome error wraps agent.ErrPermissionDenied, which the engine maps to
// permanent_failure. A bad/missing API key is deterministic; retrying it 8×
// would only stall the pipeline before the inevitable failure.
func TestLaunch_AuthFailure_Permanent(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":true,"duration_ms":71,"num_turns":1,"result":"Not logged in · Please run /login","stop_reason":"stop_sequence","session_id":"x","total_cost_usd":0}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{NodePath: "graph[0]", Uses: claude.AdapterRef, With: ir.RawConfig{"prompt": "x"}}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	for range eventCh {
	}
	outcome := <-outcomeCh
	if outcome.Err == nil {
		t.Fatal("outcome.Err = nil; want a permanent auth error")
	}
	if !errors.Is(outcome.Err, agent.ErrPermissionDenied) {
		t.Fatalf("outcome.Err = %v; want errors.Is agent.ErrPermissionDenied (auth must be permanent, not retryable)", outcome.Err)
	}
}

func TestLaunch_NoResultEvent_ErrUnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"system","subtype":"init","session_id":"s1"}
`)
	f.ProgramExecAny(container.ExecResult{ExitCode: 137, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With:     ir.RawConfig{"prompt": "x"},
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	for range eventCh {
	}
	outcome := <-outcomeCh
	var unexp *claude.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v; want *ErrUnexpectedExit", outcome.Err)
	}
	if unexp.ExitCode != 137 {
		t.Errorf("ExitCode = %d", unexp.ExitCode)
	}
}

func TestLaunch_OmitsSessionFlagsInCommandLine(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath:     "graph[0]",
		Uses:         claude.AdapterRef,
		With:         ir.RawConfig{"prompt": "x"},
		OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	// Inspect the fake's recorded Cmd.Run string.
	if len(f.Calls) == 0 {
		t.Fatal("no recorded calls")
	}
	cmd := f.Calls[0].Run
	for _, forbidden := range []string{"--continue", "--resume", "--session-id"} {
		if strings.Contains(cmd, forbidden) {
			t.Errorf("command contained forbidden flag %q: %s", forbidden, cmd)
		}
	}
	if !strings.Contains(cmd, "--no-session-persistence") {
		t.Errorf("command missing --no-session-persistence: %s", cmd)
	}
	if !strings.Contains(cmd, "--output-format stream-json") {
		t.Errorf("command missing --output-format stream-json: %s", cmd)
	}
	if !strings.Contains(cmd, "--json-schema") {
		t.Errorf("command missing --json-schema: %s", cmd)
	}
}

func TestAssembleCommand_SchemaRoundTripsThroughJSONMarshal(t *testing.T) {
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"answer", "confidence"},
		"properties": map[string]any{
			"answer":     map[string]any{"type": "integer"},
			"confidence": map[string]any{"type": "number"},
		},
	}
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"answer":42,"confidence":0.9}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath:     "graph[0]",
		Uses:         claude.AdapterRef,
		With:         ir.RawConfig{"prompt": "x"},
		OutputSchema: schema,
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	<-outcomeCh
	if len(f.Calls) == 0 {
		t.Fatal("no recorded call")
	}
	cmd := f.Calls[0].Run
	for _, fragment := range []string{`--json-schema`, `"required":["answer","confidence"]`, `"type":"object"`, `"additionalProperties":false`} {
		if !strings.Contains(cmd, fragment) {
			t.Errorf("command missing fragment %q\nfull cmd: %s", fragment, cmd)
		}
	}
}

func TestShellQuote_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "''"},
		{"plain", "hello", "'hello'"},
		{"single_quote", "it's", `'it'\''s'`},
		{"double_quote", `say "hi"`, `'say "hi"'`},
		{"dollar", "$PATH", "'$PATH'"},
		{"semicolon", "x; rm -rf /", "'x; rm -rf /'"},
		{"backslash", `path\to\file`, `'path\to\file'`},
		{"newline", "line1\nline2", "'line1\nline2'"},
		{"backtick", "`whoami`", "'`whoami`'"},
		{"mixed", `a'b"c$d;e\f`, `'a'\''b"c$d;e\f'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claude.ShellQuoteForTest(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLaunch_ErrorMaxStructuredOutputRetries_MapsToErrUnparseableOutput
// (slice 5.4 Bucket 14b unit-test relocation — revision 5 round 1).
//
// Drives the adapter's parser end-to-end against a stream-json fixture
// whose final result event carries subtype: "error_max_structured_output_retries".
// Asserts the returned error is *agent.ErrUnparseableOutput so the
// slice-5.2 dispatcher maps it to retryable_failure.
//
// Why this lives here (not in conformance.RunAgentSuite): Anthropic's
// structured-outputs API enforces schemas at decoding via forced tool
// calls (Phase 5 design Appendix H), so the error path is rare in
// practice and not reliably reproducible via prompt. The fixture is
// derived from sample-stream-success.jsonl (a real captured stream)
// by substituting only the final result event's subtype — all earlier
// events are the real captured ones, so the adapter parser sees a
// realistic prefix.
func TestLaunch_ErrorMaxStructuredOutputRetries_MapsToErrUnparseableOutput(t *testing.T) {
	fixturePath := filepath.Join("testdata", "error-max-retries-stream.jsonl")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// ProgramExecAny (not ProgramExec): the slice-5.3 adapter's
	// assembleCommand at agent/claude/launch.go:175 builds a long
	// command line like `claude -p "<prompt>" --output-format
	// stream-json --json-schema '...' --no-session-persistence ...`.
	// ProgramExec keys on EXACT cmd.Run match — would never hit. The
	// fall-through ProgramExecAny ([container/fake.go:260]) matches
	// any command the adapter sends.
	fk := container.NewFake()
	h, err := fk.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	chunks := []container.IOChunk{{Stream: "stdout", Data: fixture}}
	fk.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: fixture}, chunks)

	ad, err := claude.New(claude.WithBackend(fk), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "test"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"answer"},
		"properties": map[string]any{
			"answer": map[string]any{"type": "integer"},
		},
	}

	events, outcomeCh, lerr := ad.Launch(context.Background(), h, agent.AgentInvocation{
		NodePath:     "/test/14b-relocated",
		Uses:         "anthropic/claude-code",
		With:         ir.RawConfig{"prompt": "any"},
		OutputSchema: &schema,
	})
	if lerr != nil {
		t.Fatalf("Launch: %v", lerr)
	}
	for range events {
	}
	oc, ok := <-outcomeCh
	if !ok {
		t.Fatal("outcome channel closed without emitting")
	}

	var target *agent.ErrUnparseableOutput
	if !errors.As(oc.Err, &target) {
		t.Errorf("oc.Err = %T %v; want *agent.ErrUnparseableOutput", oc.Err, oc.Err)
	}
}

func TestAssembleCommand_EmitsFlagPerWithKey(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{{Stream: "stdout", Data: streamLines}})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With: ir.RawConfig{
			"prompt":         "x",
			"model":          "claude-opus-4-8",
			"effort":         "max",
			"max_turns":      3,
			"system_prompt":  "be terse",
			"allowed_tools":  []any{"Bash", "Read"},
			"max_budget_usd": 5,
		},
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	for key, flag := range map[string]string{
		"model":          "--model",
		"effort":         "--effort",
		"max_turns":      "--max-turns",
		"system_prompt":  "--system-prompt",
		"allowed_tools":  "--allowedTools",
		"max_budget_usd": "--max-budget-usd",
		"bare":           "--bare", // default true
	} {
		if !strings.Contains(cmd, flag) {
			t.Errorf("with[%q] did not emit %q: %s", key, flag, cmd)
		}
	}
}

func TestAssembleCommand_BareFalseOmitsFlag(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{{Stream: "stdout", Data: streamLines}})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{NodePath: "graph[0]", Uses: claude.AdapterRef, With: ir.RawConfig{"prompt": "x", "bare": false}}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if strings.Contains(f.Calls[0].Run, "--bare") {
		t.Errorf("bare=false still emitted --bare: %s", f.Calls[0].Run)
	}
}

func TestLaunch_FeedbackPrependedToPrompt(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})
	a, _ := claude.New(claude.WithBackend(f), claude.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	inv := agent.AgentInvocation{
		NodePath:     "gate[0].attempt-2.generate[0]",
		Uses:         claude.AdapterRef,
		With:         ir.RawConfig{"prompt": "write the thing"},
		OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
		Feedback:     ir.RawConfig{"verified": false, "feedback": "missing detection"},
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	<-outcomeCh
	cmd := f.Calls[0].Run
	if !strings.Contains(cmd, "previous verdict") {
		t.Errorf("command missing previous-verdict preamble: %s", cmd)
	}
	if !strings.Contains(cmd, "missing detection") {
		t.Errorf("command missing feedback content: %s", cmd)
	}
}
