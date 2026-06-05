package awfllm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func launchAdapter(t *testing.T, rt http.RoundTripper) *awfllm.Adapter {
	t.Helper()
	a, err := awfllm.New(awfllm.WithEnv(map[string]string{"OPENAI_API_KEY": "sk-test"}), awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func drainLaunch(t *testing.T, a *awfllm.Adapter, inv agent.AgentInvocation) ([]agent.AgentEvent, agent.AgentOutcome) {
	t.Helper()
	events, outcomeCh, err := a.Launch(context.Background(), container.Handle{}, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	var evs []agent.AgentEvent
	for ev := range events {
		evs = append(evs, ev)
	}
	return evs, <-outcomeCh
}

func okInv() agent.AgentInvocation {
	return agent.AgentInvocation{
		NodePath: "graph[0]", Uses: awfllm.AdapterRef,
		With:         ir.RawConfig{"model": "gpt-x", "prompt": "2+2?", "base_url": "https://x/v1"},
		OutputSchema: &ir.JSONSchema{"type": "object"},
	}
}

func TestLaunch_StreamsDeltasAndParses(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) { return sseResponse(openAISSE), nil })
	a := launchAdapter(t, rt)
	evs, outcome := drainLaunch(t, a, okInv())
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4 (reassembled from 2 deltas)", outcome.Result.Output["answer"])
	}
	// Two DisplayAssistantDelta events (char-by-char) + a terminal DisplayFinal.
	// (Launch's events channel still carries Display here — the dispatcher only
	// drops Display before journaling; this test drains Launch directly.)
	var delta, final int
	for _, ev := range evs {
		switch ev.Display.Class {
		case agent.DisplayAssistantDelta:
			delta++
		case agent.DisplayFinal:
			final++
		}
	}
	if delta != 2 || final != 1 {
		t.Errorf("events: delta=%d final=%d, want 2 and 1", delta, final)
	}
}

func TestAssemblePrompt_InjectsSchemaDirective(t *testing.T) {
	inv := agent.AgentInvocation{
		With:         ir.RawConfig{"prompt": "hi"},
		OutputSchema: &ir.JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}},
	}
	got := awfllm.AssemblePromptForTest(inv)
	if !strings.Contains(got, "hi") || !strings.Contains(got, "JSON object conforming") || !strings.Contains(got, "answer") {
		t.Errorf("assemblePrompt must keep prompt + add schema directive + schema, got %q", got)
	}
}

func TestAssemblePrompt_NoSchema_Unchanged(t *testing.T) {
	inv := agent.AgentInvocation{With: ir.RawConfig{"prompt": "hi"}}
	if got := awfllm.AssemblePromptForTest(inv); got != "hi" {
		t.Errorf("no schema → prompt unchanged, got %q", got)
	}
}

func TestAssemblePrompt_PrependsFeedback(t *testing.T) {
	inv := agent.AgentInvocation{
		With:     ir.RawConfig{"prompt": "fix it"},
		Feedback: ir.RawConfig{"verdict": "missing field answer"},
	}
	got := awfllm.AssemblePromptForTest(inv)
	if !strings.Contains(got, "<previous verdict>") || !strings.Contains(got, "missing field answer") || !strings.Contains(got, "fix it") {
		t.Errorf("repair attempt must prepend prior verdict + keep prompt, got %q", got)
	}
}

func TestLaunch_NoSchema_NilOutput(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) { return sseResponse(openAISSE), nil })
	a := launchAdapter(t, rt)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: awfllm.AdapterRef,
		With: ir.RawConfig{"model": "gpt-x", "prompt": "2+2?", "base_url": "https://x/v1"},
	}
	_, outcome := drainLaunch(t, a, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if outcome.Result.Output != nil {
		t.Errorf("OutputSchema==nil → Output must be nil, got %v", outcome.Result.Output)
	}
}

func TestLaunch_CtxCancelMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Body yields one delta, then blocks until the request ctx is cancelled and
	// closes the pipe with the ctx error — exercising mid-stream cancellation.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.WriteString(pw, "data: {\"choices\":[{\"delta\":{\"content\":\"{\"}}]}\n\n")
			<-r.Context().Done()
			_ = pw.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}, nil
	})
	a := launchAdapter(t, rt)
	events, outcomeCh, err := a.Launch(ctx, container.Handle{}, okInv())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Drain events; signal once the first delta has actually been read off the
	// wire. Cancelling only AFTER the reader has consumed the first chunk avoids
	// deadlocking the fake transport's pipe writer (which would otherwise block
	// forever on a write nobody reads, leaking a goroutine — goleak would catch it).
	firstDelta := make(chan struct{}, 1)
	go func() {
		for ev := range events { //nolint:revive // drain
			if ev.Display.Class == agent.DisplayAssistantDelta {
				select {
				case firstDelta <- struct{}{}:
				default:
				}
			}
		}
	}()
	<-firstDelta
	cancel()
	outcome := <-outcomeCh
	var launchErr *agent.ErrAgentLaunch
	if outcome.Err == nil || !errorsAs(outcome.Err, &launchErr) {
		t.Fatalf("ctx-cancel outcome.Err = %v, want *agent.ErrAgentLaunch (retryable)", outcome.Err)
	}
	// goleak (main_test) asserts the Launch goroutine exited cleanly.
}

func TestLaunch_400Permanent(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))}, nil
	})
	a := launchAdapter(t, rt)
	// spec §B.7 step 4: a mid-stream error must emit a DisplayError event before
	// the outcome, so the live renderer can terminate the in-progress delta line
	// and display the error prominently.
	evs, outcome := drainLaunch(t, a, okInv())
	var bad *agent.ErrInvalidConfig
	if outcome.Err == nil || !errorsAs(outcome.Err, &bad) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrInvalidConfig (permanent)", outcome.Err)
	}
	var hasDisplayError bool
	for _, ev := range evs {
		if ev.Display.Class == agent.DisplayError && ev.Display.IsError {
			hasDisplayError = true
			break
		}
	}
	if !hasDisplayError {
		t.Errorf("stream error must emit a DisplayError event before outcome; events: %v", evs)
	}
}

func TestLaunch_429Retryable(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow"}}`))}, nil
	})
	a := launchAdapter(t, rt)
	_, outcome := drainLaunch(t, a, okInv())
	var launchErr *agent.ErrAgentLaunch
	if outcome.Err == nil || !errorsAs(outcome.Err, &launchErr) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch (retryable)", outcome.Err)
	}
}

func TestLaunch_Truncation_Unparseable(t *testing.T) {
	// finish_reason length + truncated JSON → retryable ErrUnparseableOutput.
	const truncated = `data: {"choices":[{"index":0,"delta":{"content":"{\"answer\":"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}

data: [DONE]

`
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) { return sseResponse(truncated), nil })
	a := launchAdapter(t, rt)
	_, outcome := drainLaunch(t, a, okInv())
	var unp *agent.ErrUnparseableOutput
	if outcome.Err == nil || !errorsAs(outcome.Err, &unp) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrUnparseableOutput", outcome.Err)
	}
}

func TestLaunch_ProseAndFence_ExtractsJSON(t *testing.T) {
	// Deltas carry prose + a fenced code block; extractJSONObject recovers the object.
	const proseSSE = `data: {"choices":[{"index":0,"delta":{"content":"Sure! "}}]}

data: {"choices":[{"index":0,"delta":{"content":"` + "```" + `json\n"}}]}

data: {"choices":[{"index":0,"delta":{"content":"{\"answer\":4}"}}]}

data: {"choices":[{"index":0,"delta":{"content":"\n` + "```" + `"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}

data: [DONE]

`
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) { return sseResponse(proseSSE), nil })
	a := launchAdapter(t, rt)
	_, outcome := drainLaunch(t, a, okInv())
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4 (recovered from prose+fence)", outcome.Result.Output["answer"])
	}
}

func TestLaunch_HappyMetrics(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) { return sseResponse(openAISSE), nil })
	a := launchAdapter(t, rt)
	_, outcome := drainLaunch(t, a, okInv())
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	m := outcome.Result.Metrics
	if m.Tokens.Input != 20 || m.Tokens.Output != 5 {
		t.Errorf("tokens = %+v, want In:20 Out:5", m.Tokens)
	}
	if m.Cost.USD != 0 {
		t.Errorf("cost.USD = %v, want 0 (no pricing pkg)", m.Cost.USD)
	}
	if m.Turns != 1 {
		t.Errorf("turns = %d, want 1", m.Turns)
	}
}
