package codexlive

import (
	"encoding/json"
	"testing"
)

func TestThreadStartResponseDecodesResolvedModel(t *testing.T) {
	// thread/start carries the RESOLVED model at the TOP LEVEL (a required,
	// non-null string even when the request omitted model) — NOT inside `thread`.
	var resp threadStartResponse
	if err := json.Unmarshal([]byte(`{"thread":{"id":"thread-1","path":"/tmp/t.jsonl","sessionId":"sess-1"},"model":"gpt-5.3-codex"}`), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := threadInfoFromResponse(resp.Thread, resp.Model)
	if info.Model != "gpt-5.3-codex" {
		t.Fatalf("ThreadInfo.Model = %q, want gpt-5.3-codex", info.Model)
	}
	if info.ID != "thread-1" {
		t.Fatalf("ThreadInfo.ID = %q, want thread-1", info.ID)
	}
}

func TestProcessClientBuffersEarlyTurnEventsAndRequests(t *testing.T) {
	c := &processClient{}
	c.handleLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}}`))
	c.handleLine([]byte(`{"jsonrpc":"2.0","id":"request-1","method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-2","command":"go test ./...","cwd":"/tmp/work","reason":"test"}}`))
	c.handleLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1"}}}`))

	events := c.registerTurnEvents("turn-1")
	var got []ProviderEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("buffered events = %d, want 3: %+v", len(got), got)
	}
	if got[0].Type != EventAgentMessageDelta || got[0].Text != "hello" {
		t.Fatalf("first event = %+v, want agent delta", got[0])
	}
	if got[1].Type != EventPermissionRequest || got[1].Permission == nil {
		t.Fatalf("second event = %+v, want permission request", got[1])
	}
	if got[1].Permission.ID != "request-1" || got[1].Permission.TurnID != "turn-1" || got[1].Permission.Command != "go test ./..." {
		t.Fatalf("permission event = %+v, want routed request", got[1].Permission)
	}
	if got[2].Type != EventTurnCompleted {
		t.Fatalf("third event = %+v, want turn completed", got[2])
	}
	if c.requestKinds["request-1"] != serverRequestCommandApproval {
		t.Fatalf("request kind = %q, want %q", c.requestKinds["request-1"], serverRequestCommandApproval)
	}
}

func TestProviderEventFromNotificationCarriesTokenUsage(t *testing.T) {
	// thread/tokenUsage/updated carries `last` (this turn) and `total` (cumulative
	// thread). Per-step AWF metrics are per-turn, so we take `last`, not `total`.
	params := []byte(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{` +
		`"last":{"inputTokens":1200,"outputTokens":340,"cachedInputTokens":800,"reasoningOutputTokens":50,"totalTokens":1540},` +
		`"total":{"inputTokens":5000,"outputTokens":900,"cachedInputTokens":3000,"reasoningOutputTokens":120,"totalTokens":5900}}}`)
	ev, turnID, closeTurn, ok := providerEventFromNotification(EventThreadTokenUsage, params)
	if !ok {
		t.Fatal("providerEventFromNotification ok = false")
	}
	if closeTurn {
		t.Fatal("token-usage update must not close the turn")
	}
	if turnID != "turn-1" {
		t.Fatalf("turnID = %q, want turn-1", turnID)
	}
	if ev.Type != EventThreadTokenUsage {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, EventThreadTokenUsage)
	}
	if ev.Usage.InputTokens != 1200 || ev.Usage.OutputTokens != 340 || ev.Usage.CachedInputTokens != 800 {
		t.Fatalf("ev.Usage = %+v, want {Input:1200 Output:340 Cached:800} from `last`", ev.Usage)
	}
}

func TestProviderEventFromNotificationForwardsReasoningSummaryDelta(t *testing.T) {
	// item/reasoning/summaryTextDelta is the ONLY liveness signal codex exposes
	// during reasoning. It must map to a non-terminal ProviderEvent (closeTurn
	// false) carrying the delta text so the drain loop can beat the idle timer.
	params := []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"analyzing the repo"}`)
	ev, turnID, closeTurn, ok := providerEventFromNotification(EventReasoningSummaryDelta, params)
	if !ok {
		t.Fatal("providerEventFromNotification ok = false")
	}
	if closeTurn {
		t.Fatal("reasoning-summary delta must not close the turn")
	}
	if turnID != "turn-1" {
		t.Fatalf("turnID = %q, want turn-1", turnID)
	}
	if ev.Type != EventReasoningSummaryDelta {
		t.Fatalf("ev.Type = %q, want %q", ev.Type, EventReasoningSummaryDelta)
	}
	if ev.Text != "analyzing the repo" {
		t.Fatalf("ev.Text = %q, want %q", ev.Text, "analyzing the repo")
	}
}

func TestProviderEventFromNotificationCarriesTurnFailureStatus(t *testing.T) {
	ev, turnID, closeTurn, ok := providerEventFromNotification(EventTurnCompleted, []byte(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed","error":{"message":"boom","codexErrorInfo":null,"additionalDetails":null}}}`))
	if !ok {
		t.Fatal("providerEventFromNotification ok = false")
	}
	if turnID != "turn-1" || !closeTurn {
		t.Fatalf("turnID=%q closeTurn=%v, want turn-1 true", turnID, closeTurn)
	}
	if ev.Type != EventTurnCompleted || ev.Status != "failed" || ev.Error != "boom" {
		t.Fatalf("event = %+v, want failed turn with message", ev)
	}
}
