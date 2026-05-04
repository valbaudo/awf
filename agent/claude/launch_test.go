package claude_test

import (
	"context"
	"errors"
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
	if outcome.Result.Metrics.Cost.USD != 0.001 {
		t.Errorf("Cost.USD = %v", outcome.Result.Metrics.Cost.USD)
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
