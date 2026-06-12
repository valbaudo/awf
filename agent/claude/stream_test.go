package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// loadFixture reads testdata/<name>.jsonl and returns its non-empty lines.
// Captured from real claude 2.1.153 sessions per Phase 5 design Appendix A.
func loadFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 && line[0] == '{' {
			out := make([]byte, len(line))
			copy(out, line)
			lines = append(lines, out)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return lines
}

// Synthetic small-surface fixtures for the focused per-event-type tests.
const (
	systemInitLine = `{"type":"system","subtype":"init","session_id":"sess-1","cwd":"/tmp","model":"claude-opus-4-7","tools":["Bash","Read"],"claude_code_version":"2.1.152"}`

	assistantSingleTextLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`

	assistantMultiBlockLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think"},{"type":"text","text":"my answer"},{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"echo"}}]}}`

	rateLimitLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1735689600,"rateLimitType":"seven_day","utilization":0.85,"isUsingOverage":false,"surpassedThreshold":0.8}}`

	resultSuccessLine = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"ttft_ms":500,"num_turns":1,"result":"final text","stop_reason":"end_turn","total_cost_usd":0.012,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"structured_output":{"answer":42}}`

	resultErrorLine = `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"duration_ms":5000,"num_turns":3}`

	// auth-failure case: subtype:success but is_error:true. Pre-fix parser
	// returned (AgentResult{}, nil) silently — now returns ErrAuthFailureSentinel.
	resultAuthFailureLine = `{"type":"result","subtype":"success","is_error":true,"duration_ms":71,"num_turns":1,"result":"Not logged in · Please run /login","stop_reason":"stop_sequence","session_id":"x","total_cost_usd":0}`
)

func TestParseStreamLine_SystemInit_FromCapturedFixture(t *testing.T) {
	lines := loadFixture(t, "sample-stream.jsonl")
	if len(lines) == 0 {
		t.Fatal("no lines in sample-stream.jsonl")
	}
	msg, err := parseStreamLine(lines[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.Type != "system" || msg.Subtype != "init" {
		t.Errorf("Type/Subtype = %q/%q", msg.Type, msg.Subtype)
	}
	if msg.SessionID == "" {
		t.Error("SessionID empty")
	}
	if msg.Version == "" {
		t.Error("claude_code_version empty")
	}
}

func TestExtractResult_AuthFailureSubtypeSuccessIsError(t *testing.T) {
	msg, err := parseStreamLine([]byte(resultAuthFailureLine))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, eerr := extractResult(msg, "")
	if eerr == nil {
		t.Fatal("extractResult returned nil error; want ErrAuthFailureSentinel")
	}
	if !errors.Is(eerr, ErrAuthFailureSentinel) {
		t.Errorf("extractResult error = %v; want errors.Is ErrAuthFailureSentinel", eerr)
	}
	if !strings.Contains(eerr.Error(), "Not logged in") {
		t.Errorf("error = %v; want it to wrap claude's 'Not logged in' message", eerr)
	}
}

func TestParseStreamLine_BadJSON(t *testing.T) {
	_, err := parseStreamLine([]byte(`not json`))
	if err == nil {
		t.Fatal("err nil; want non-nil")
	}
}

func TestMessageToEvents_AssistantSingleText(t *testing.T) {
	msg, _ := parseStreamLine([]byte(assistantSingleTextLine))
	events := messageToEvents(msg)
	if len(events) != 1 {
		t.Fatalf("len = %d, want 1", len(events))
	}
	if events[0].Kind != "text" {
		t.Errorf("Kind = %q, want %q", events[0].Kind, "text")
	}
	if !strings.Contains(string(events[0].Payload), "hello") {
		t.Errorf("Payload = %q; want it to contain 'hello'", events[0].Payload)
	}
}

func TestMessageToEvents_AssistantMultiBlock_Splits(t *testing.T) {
	msg, _ := parseStreamLine([]byte(assistantMultiBlockLine))
	events := messageToEvents(msg)
	if len(events) != 3 {
		t.Fatalf("len = %d, want 3 (thinking + text + tool_use)", len(events))
	}
	kinds := []string{events[0].Kind, events[1].Kind, events[2].Kind}
	want := []string{"thinking", "text", "tool_use"}
	for i := range kinds {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
}

func TestMessageToEvents_SystemEmitsSingleEvent(t *testing.T) {
	msg, _ := parseStreamLine([]byte(systemInitLine))
	events := messageToEvents(msg)
	if len(events) != 1 {
		t.Fatalf("len = %d, want 1", len(events))
	}
	if events[0].Kind != "system" {
		t.Errorf("Kind = %q", events[0].Kind)
	}
}

func TestMessageToEvents_RateLimit(t *testing.T) {
	msg, _ := parseStreamLine([]byte(rateLimitLine))
	events := messageToEvents(msg)
	if len(events) != 1 {
		t.Fatalf("len = %d, want 1", len(events))
	}
	if events[0].Kind != "rate_limit" {
		t.Errorf("Kind = %q", events[0].Kind)
	}
}

func TestExtractResult_Success(t *testing.T) {
	msg, _ := parseStreamLine([]byte(resultSuccessLine))
	res, err := extractResult(msg, "")
	if err != nil {
		t.Fatalf("extractResult: %v", err)
	}
	if res.Metrics.Cost.Total != 0.012 {
		t.Errorf("Cost.Total = %v, want 0.012", res.Metrics.Cost.Total)
	}
	if res.Metrics.Tokens.Input != 100 {
		t.Errorf("Tokens.Input = %d", res.Metrics.Tokens.Input)
	}
	if res.Metrics.Tokens.Output != 50 {
		t.Errorf("Tokens.Output = %d", res.Metrics.Tokens.Output)
	}
	if res.Metrics.Turns != 1 {
		t.Errorf("Turns = %d", res.Metrics.Turns)
	}
	if res.Output["answer"] == nil {
		t.Fatal("Output[answer] missing")
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 42 {
		t.Errorf("Output[answer] = %v (%T), want 42", v, res.Output["answer"])
	}
}

func TestExtractResult_ErrorMaxStructuredOutputRetries(t *testing.T) {
	msg, _ := parseStreamLine([]byte(resultErrorLine))
	_, err := extractResult(msg, "")
	if err == nil {
		t.Fatal("err nil; want non-nil for error_max_structured_output_retries")
	}
	if !strings.Contains(err.Error(), "structured_output") {
		t.Errorf("err = %v; want mention of structured_output", err)
	}
}

func TestExtractResult_CapturesSystemInitModel(t *testing.T) {
	// Simulate the streaming loop: parse system/init to capture the model,
	// then parse the result event and assert Metrics.Model is populated.
	initMsg, err := parseStreamLine([]byte(systemInitLine))
	if err != nil {
		t.Fatalf("parse system/init: %v", err)
	}
	if initMsg.Type != "system" || initMsg.Subtype != "init" {
		t.Fatalf("unexpected message type: %s/%s", initMsg.Type, initMsg.Subtype)
	}
	capturedModel := initMsg.Model // "claude-opus-4-7"

	resultMsg, err := parseStreamLine([]byte(resultSuccessLine))
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	res, err := extractResult(resultMsg, capturedModel)
	if err != nil {
		t.Fatalf("extractResult: %v", err)
	}
	if res.Metrics.Model != "claude-opus-4-7" {
		t.Errorf("Metrics.Model = %q, want %q", res.Metrics.Model, "claude-opus-4-7")
	}
}

func TestStreamMessage_RoundTrip(t *testing.T) {
	in := streamMessage{
		Type:    "system",
		Subtype: "init",
	}
	b, _ := json.Marshal(in)
	var out streamMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Type != in.Type || out.Subtype != in.Subtype {
		t.Errorf("roundtrip drifted: %+v vs %+v", out, in)
	}
}
