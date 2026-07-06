package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// loadAgentSimpleDef is a test helper — writes the YAML to a temp file, loads
// it, and returns the LoadedDefinition. (Mirrors patterns from engine/interpreter_test.go.)
func loadAgentSimpleDef(t *testing.T, yaml string) *ir.LoadedDefinition {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/wf.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	ld, err := loader.Load(path)
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("ir.Validate: %v", diags)
	}
	return ld
}

func TestRunAgentStep_HappyPath_TemplatedWith(t *testing.T) {
	const yaml = `workflow: agent-step-templated
version: 1
input_schema:
  type: object
  required: [topic]
  additionalProperties: false
  properties:
    topic: { type: string }
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "summarize {{ input.topic }}"
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"verdict": "approved"},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	handles := map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}}
	dispatcher := &engine.LocalDispatcher{Backend: be, Handles: handles, Resolver: &reg}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", map[string]any{"topic": "frogs"})

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(calls))
	}
	if got := calls[0].With["prompt"]; got != "summarize frogs" {
		t.Errorf("With[prompt] = %v, want %q (template not substituted)", got, "summarize frogs")
	}

	nr, ok := rs.LookupCompleted("triage")
	if !ok {
		t.Fatalf("Completed[triage] missing")
	}
	if nr.Outputs["verdict"] != "approved" {
		t.Errorf("Outputs[verdict] = %v, want %q", nr.Outputs["verdict"], "approved")
	}
}

func TestAgentInvocationReceivesRunContextCurrentAndNextEpoch(t *testing.T) {
	const yaml = `workflow: agent-run-context
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p"
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "run-context", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("run-context", "d", nil)
	rs.Epoch = 7

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(calls))
	}
	want := agent.RunContext{RunID: "run-context", CurrentEpoch: 7, NextEpoch: 7}
	if calls[0].RunContext != want {
		t.Fatalf("AgentInvocation.RunContext = %+v, want %+v", calls[0].RunContext, want)
	}
}

func TestRunAgentStep_AgentEventLogEntries(t *testing.T) {
	const yaml = `workflow: agent-events
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{
			{Kind: "system", Payload: []byte(`{"subtype":"init"}`)},
			{Kind: "result", Payload: []byte(`{"subtype":"success"}`)},
		},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	count := 0
	for _, ev := range events {
		if ev.Type == engine.EventAgentEvent && ev.Path == "triage" {
			count++
			var data engine.AgentEventData
			if uerr := json.Unmarshal(ev.Data, &data); uerr != nil {
				t.Fatalf("unmarshal agent.event Data: %v", uerr)
			}
			if data.Kind == "" {
				t.Errorf("AgentEventData.Kind empty in %+v", data)
			}
		}
	}
	if count != 2 {
		t.Errorf("agent.event log entry count = %d, want 2", count)
	}
}

func TestRunAgentStep_LiveAgentEventDisplayMetadataSurvivesDispatcher(t *testing.T) {
	const yaml = `workflow: live-agent-events
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: live
    container: lab
    uses: openai/codex-live
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("openai/codex-live").WithCaps(agent.Caps{NativeSchema: true, PersistentSession: true, IsolatedConfigDir: true}).Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{{
			Kind:    "delta",
			Stream:  "stdout",
			Live:    true,
			Payload: []byte("hello sk-liveSECRET123"),
			Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "hello sk-liveSECRET123"},
		}},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  &autoTreeFake{Fake: container.NewFake()},
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type != engine.EventAgentEvent || ev.Path != "live" {
			continue
		}
		var data engine.AgentEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("unmarshal AgentEventData: %v", err)
		}
		if data.DisplayClass != "assistant_delta" || data.DisplaySummary != "hello sk-[redacted]" {
			t.Fatalf("display metadata = class %q summary %q, want assistant_delta/hello sk-[redacted]", data.DisplayClass, data.DisplaySummary)
		}
		return
	}
	t.Fatal("missing live agent.event")
}

func TestRunAgentStep_LiveAgentRetryBoundaryFlushesFailedAttemptBeforeRetry(t *testing.T) {
	const yaml = `workflow: live-agent-retry-boundary
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: live
    container: lab
    uses: openai/codex-live
    retry: { attempts: 2, backoff: none }
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("openai/codex-live").Script(0, fake.Result{
		Output: map[string]any{"ok": "nope"},
		Events: []agent.AgentEvent{{
			Kind:    "delta",
			Stream:  "stdout",
			Live:    true,
			Payload: []byte("first"),
			Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "first"},
		}},
	}).Script(1, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{{
			Kind:    "delta",
			Stream:  "stdout",
			Live:    true,
			Payload: []byte("second"),
			Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "second"},
		}},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	log := state.NewInMemoryLog(clock.System{})
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clock.System{}, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var order []string
	for _, ev := range events {
		if ev.Path != "live" {
			continue
		}
		switch ev.Type {
		case engine.EventAgentEvent:
			var data engine.AgentEventData
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("unmarshal AgentEventData: %v", err)
			}
			order = append(order, "agent.event:"+string(data.PayloadInline))
		case engine.EventRetryAttempt:
			order = append(order, "retry.attempt")
		}
	}
	want := []string{"agent.event:first", "retry.attempt", "agent.event:second"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
}

func TestRunAgentStep_LiveAgentEventPreservesDisplayLinesAndBytes(t *testing.T) {
	const yaml = `workflow: live-agent-display-counts
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: live
    container: lab
    uses: openai/codex-live
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("openai/codex-live").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{{
			Kind:    "tool_result",
			Stream:  "stdout",
			Live:    true,
			Payload: []byte("line1\nline2\nline3\n"),
			Display: agent.EventDisplay{
				Class:   agent.DisplayToolResult,
				Tool:    "shell",
				Text:    "line1\n...\nline3",
				Lines:   3,
				Bytes:   len("line1\nline2\nline3\n"),
				IsError: true,
			},
		}},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type != engine.EventAgentEvent || ev.Path != "live" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(ev.Data, &raw); err != nil {
			t.Fatalf("unmarshal raw agent.event JSON: %v", err)
		}
		if got := raw["display_lines"]; got != float64(3) {
			t.Fatalf("display_lines = %v, want 3", got)
		}
		if got := raw["display_bytes"]; got != float64(len("line1\nline2\nline3\n")) {
			t.Fatalf("display_bytes = %v, want %d", got, len("line1\nline2\nline3\n"))
		}
		return
	}
	t.Fatal("missing live agent.event")
}

func TestRunAgentStep_LiveAgentEventVisibleBeforeNodeCompleted(t *testing.T) {
	const yaml = `workflow: live-agent-events-inflight
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: live
    container: lab
    uses: openai/codex-live
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("openai/codex-live").
		WithCaps(agent.Caps{NativeSchema: true, PersistentSession: true, IsolatedConfigDir: true}).
		WithEmitDelay(300*time.Millisecond).
		Script(0, fake.Result{
			Output: map[string]any{"ok": true},
			Events: []agent.AgentEvent{{
				Kind:    "delta",
				Stream:  "stdout",
				Live:    true,
				Payload: []byte("hello"),
				Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "hello"},
			}},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  &autoTreeFake{Fake: container.NewFake()},
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	runDir := t.TempDir()
	log, err := state.OpenLogExclusive(filepath.Join(runDir, "log"), clk)
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)
	done := make(chan error, 1)
	go func() {
		oc, runErr := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
		if runErr != nil {
			done <- runErr
			return
		}
		if oc != engine.OutcomeOK {
			done <- fmt.Errorf("Outcome = %q, want %q", oc, engine.OutcomeOK)
			return
		}
		done <- nil
	}()

	deadline := time.After(2 * time.Second)
	for {
		events, err := log.Fold()
		if err != nil {
			t.Fatalf("Fold: %v", err)
		}
		var seenLiveEvent, seenCompleted bool
		for _, ev := range events {
			switch ev.Type {
			case engine.EventAgentEvent:
				if ev.Path == "live" {
					seenLiveEvent = true
				}
			case engine.EventNodeCompleted:
				if ev.Path == "live" {
					seenCompleted = true
				}
			}
		}
		if seenLiveEvent && !seenCompleted {
			break
		}
		if seenCompleted {
			t.Fatal("node.completed appeared before a separately observable live agent.event")
		}
		select {
		case err := <-done:
			t.Fatalf("engine.Run completed before live agent.event became visible; err=%v", err)
		case <-deadline:
			t.Fatal("timed out waiting for live agent.event before node.completed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
}

func TestRunAgentStep_LiveReplayRequiredLaunchErrHaltsWithoutFailureOrRetry(t *testing.T) {
	const yaml = `workflow: live-replay-launch
version: 1
graph:
  - id: live
    uses: live/agent
    with:
      prompt: "p"
`
	ld := loadAgentSimpleDef(t, yaml)

	ad := &replayRequiredAdapter{
		ref:       "live/agent",
		launchErr: fmt.Errorf("wrapped: %w", agent.ErrLiveReplayRequired),
	}
	var reg agent.Registry
	if err := reg.Register(ad); err != nil {
		t.Fatalf("Register: %v", err)
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)
	dispatcher := &engine.LocalDispatcher{Resolver: &reg}

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal halt (err: %v)", oc, err)
	}
	if !errors.Is(err, agent.ErrLiveReplayRequired) {
		t.Fatalf("err = %v, want ErrLiveReplayRequired", err)
	}
	if ad.calls != 1 {
		t.Fatalf("Launch calls = %d, want 1 (no retry)", ad.calls)
	}
	assertNoEventTypeAtPath(t, log, engine.EventRetryAttempt, "live")
	assertNoEventTypeAtPath(t, log, engine.EventNodeFailed, "live")
}

func TestRunAgentStep_LiveReplayRequiredOutcomeErrAppendsPriorEventsThenHalts(t *testing.T) {
	const yaml = `workflow: live-replay-outcome
version: 1
graph:
  - id: live
    uses: live/agent
    with:
      prompt: "p"
`
	ld := loadAgentSimpleDef(t, yaml)

	ad := &replayRequiredAdapter{
		ref:        "live/agent",
		outcomeErr: fmt.Errorf("wrapped: %w", agent.ErrLiveReplayRequired),
		events: []agent.AgentEvent{
			{Kind: "assistant", Stream: "stdout", Payload: []byte(`{"delta":"working"}`)},
		},
	}
	var reg agent.Registry
	if err := reg.Register(ad); err != nil {
		t.Fatalf("Register: %v", err)
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)
	dispatcher := &engine.LocalDispatcher{Resolver: &reg}

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal halt (err: %v)", oc, err)
	}
	if !errors.Is(err, agent.ErrLiveReplayRequired) {
		t.Fatalf("err = %v, want ErrLiveReplayRequired", err)
	}
	if ad.calls != 1 {
		t.Fatalf("Launch calls = %d, want 1 (no retry)", ad.calls)
	}
	assertNoEventTypeAtPath(t, log, engine.EventRetryAttempt, "live")
	assertNoEventTypeAtPath(t, log, engine.EventNodeFailed, "live")
	if got := countEvents(t, log, engine.EventAgentEvent, "live"); got != 1 {
		t.Fatalf("agent.event count = %d, want 1 prior event before halt", got)
	}
}

func TestRunAgentStep_LiveSchemaMismatchHaltsWithoutFailureOrRetry(t *testing.T) {
	const yaml = `workflow: live-schema-replay
version: 1
graph:
  - id: live
    uses: live/agent
    with:
      prompt: "p"
    retry:
      attempts: 3
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)

	liveMeta := &agent.LiveDispatch{
		AdapterRef:     "live/agent",
		SessionKey:     "builder",
		SessionKeyHash: "sha256:session",
		LeaseID:        "lease-1",
		ActiveTurnID:   "turn-1",
		ProviderTurnID: "provider-turn-1",
		RunID:          "r1",
		NodePath:       "live",
		Epoch:          1,
		CommittedUnix:  1,
	}
	fk := fake.New("live/agent").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true, PersistentSession: true}).
		Script(0, fake.Result{Output: map[string]any{"ok": "not-bool"}, Live: liveMeta})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d", nil)
	dispatcher := &engine.LocalDispatcher{Resolver: &reg}

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal halt (err: %v)", oc, err)
	}
	if !errors.Is(err, agent.ErrLiveReplayRequired) {
		t.Fatalf("err = %v, want ErrLiveReplayRequired", err)
	}
	if calls := fk.Calls(); len(calls) != 1 {
		t.Fatalf("Launch calls = %d, want 1 (no retry after live schema mismatch)", len(calls))
	}
	assertNoEventTypeAtPath(t, log, engine.EventRetryAttempt, "live")
	assertNoEventTypeAtPath(t, log, engine.EventNodeFailed, "live")
}

func TestRunAgentStep_LiveFinalizerRunsAfterNodeCompletedSync(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)
	rs.Epoch = 4
	live := engine.LiveDispatchRecord{
		AdapterRef:     "live/agent",
		SessionKeyHash: "session-hash",
		LeaseID:        "lease-1",
		ActiveTurnID:   "turn-1",
		ProviderTurnID: "provider-1",
		RunID:          "r1",
		NodePath:       "live",
		Epoch:          4,
	}
	dispatcher := staticDispatcher{
		dr: engine.DispatchResult{Outcome: engine.OutcomeOK, Live: &live},
	}

	var got engine.LiveDispatchRecord
	sawCompletedInFinalizer := false
	oc, err := engine.Run(context.Background(), singleAgentDefinition(), rs, dispatcher, log, blobs, clk, engine.RunOptions{
		Tap: io.Discard,
		LiveFinalizer: func(_ context.Context, rec engine.LiveDispatchRecord) error {
			got = rec
			events, foldErr := log.Fold()
			if foldErr != nil {
				t.Fatalf("Fold inside finalizer: %v", foldErr)
			}
			for _, ev := range events {
				if ev.Type == engine.EventNodeCompleted && ev.Path == "live" {
					sawCompletedInFinalizer = true
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	if !sawCompletedInFinalizer {
		t.Fatal("finalizer ran before node.completed was visible in the log")
	}
	if !reflect.DeepEqual(got, live) {
		t.Fatalf("LiveFinalizer record = %+v, want %+v", got, live)
	}
}

func TestRunAgentStep_LiveFinalizerFailureLeavesNodeCommittedWithoutNodeFailed(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)
	live := engine.LiveDispatchRecord{
		AdapterRef:     "live/agent",
		SessionKeyHash: "session-hash",
		LeaseID:        "lease-1",
		ActiveTurnID:   "turn-1",
		ProviderTurnID: "provider-1",
		RunID:          "r1",
		NodePath:       "live",
		Epoch:          1,
	}
	dispatcher := staticDispatcher{
		dr: engine.DispatchResult{Outcome: engine.OutcomeOK, Live: &live},
	}
	var tap strings.Builder

	oc, err := engine.Run(context.Background(), singleAgentDefinition(), rs, dispatcher, log, blobs, clk, engine.RunOptions{
		Tap: &tap,
		LiveFinalizer: func(context.Context, engine.LiveDispatchRecord) error {
			return errors.New("finalizer failed sk-liveSECRET123")
		},
	})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	if _, ok := rs.LookupCompleted("live"); !ok {
		t.Fatal("RunState.Completed missing live after finalizer failure")
	}
	if got := countEvents(t, log, engine.EventNodeCompleted, "live"); got != 1 {
		t.Fatalf("node.completed count = %d, want 1", got)
	}
	assertNoEventTypeAtPath(t, log, engine.EventNodeFailed, "live")

	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	foundWarning := false
	for _, ev := range events {
		if ev.Type != engine.EventAgentEvent || ev.Path != "live" {
			continue
		}
		var data engine.AgentEventData
		if uerr := json.Unmarshal(ev.Data, &data); uerr != nil {
			t.Fatalf("unmarshal AgentEventData: %v", uerr)
		}
		if data.Kind == "live.finalizer.warning" &&
			data.Live &&
			data.DisplayClass == "notice" &&
			data.DisplaySummary == "finalizer failed sk-[redacted]" &&
			strings.Contains(string(data.PayloadInline), "finalizer failed sk-[redacted]") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatal("missing live DisplayNotice finalizer warning after finalizer failure")
	}
	if got := tap.String(); !strings.Contains(got, "· live finalizer: finalizer failed sk-[redacted]") {
		t.Fatalf("tap = %q, want redacted live finalizer warning", got)
	}
}

func TestRunAgentStep_ThreadAssembledFromContinues(t *testing.T) {
	// Two sequential agent steps; turn2 continues: turn1. Both use the same
	// uses: so AWF1028 (same-runtime) passes. The fake scripts a distinct
	// verbatim transcript pair per call. After the run, turn2's invocation
	// must carry turn1's committed pair as its single Thread entry, and
	// turn1's invocation must have an empty Thread.
	const yaml = `workflow: continues-linear
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: turn1
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p1"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
  - id: turn2
    container: lab
    uses: anthropic/claude-code
    continues: turn1
    with:
      prompt: "p2"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		// Threaded:true — continues: requires a Threaded adapter; the engine
		// guard (defense-in-depth) will reject a non-Threaded adapter when
		// inv.Thread is non-empty. The real claude-code adapter sets Threaded:true.
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
		Script(0, fake.Result{
			Output:     map[string]any{"k": "v1"},
			Transcript: agent.ThreadTurn{User: "prompt-1", Assistant: "answer-1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"k": "v2"},
			Transcript: agent.ThreadTurn{User: "prompt-2", Assistant: "answer-2"},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}
	// turn1: no continues -> empty Thread.
	if len(calls[0].Thread) != 0 {
		t.Errorf("turn1 Thread = %+v, want empty", calls[0].Thread)
	}
	// turn2: Thread is the single committed turn1 pair (root->current order).
	if len(calls[1].Thread) != 1 {
		t.Fatalf("turn2 Thread len = %d, want 1", len(calls[1].Thread))
	}
	if calls[1].Thread[0].User != "prompt-1" || calls[1].Thread[0].Assistant != "answer-1" {
		t.Errorf("turn2 Thread[0] = %+v, want {prompt-1,answer-1}", calls[1].Thread[0])
	}
}

// TestRunAgentStep_AssemblesThreadRootToCurrent pins the 3-turn root→current
// ordering invariant (Task 5.2). A linear chain:
//
//	t1 (no continues) → t2 continues:t1 → t3 continues:t2
//
// All three share the same adapter so a single fake handles them in call
// order 0→1→2. After a clean engine.Run:
//   - calls[0].Thread (t1) is empty (no predecessor).
//   - calls[1].Thread (t2) is exactly [{u1,a1}] — t1's committed pair only.
//   - calls[2].Thread (t3) is exactly [{u1,a1},{u2,a2}] — t1 BEFORE t2
//     (root→current, oldest first).
//
// This test PINS assembly order — if the order is reversed or a turn is
// missing, that is a real assembly bug; do not weaken the assertion.
func TestRunAgentStep_AssemblesThreadRootToCurrent(t *testing.T) {
	const yaml = `workflow: continues-3turn
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: t1
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p1"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
  - id: t2
    container: lab
    uses: anthropic/claude-code
    continues: t1
    with:
      prompt: "p2"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
  - id: t3
    container: lab
    uses: anthropic/claude-code
    continues: t2
    with:
      prompt: "p3"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		// Threaded:true — continues: requires a Threaded adapter; the engine
		// guard (defense-in-depth) will reject a non-Threaded adapter when
		// inv.Thread is non-empty. The real claude-code adapter sets Threaded:true.
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
		Script(0, fake.Result{
			Output:     map[string]any{"x": "v1"},
			Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"x": "v2"},
			Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"},
		}).
		Script(2, fake.Result{
			Output:     map[string]any{"x": "v3"},
			Transcript: agent.ThreadTurn{User: "u3", Assistant: "a3"},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 3 {
		t.Fatalf("len(calls) = %d, want 3", len(calls))
	}

	// t1: no continues → Thread must be empty.
	if len(calls[0].Thread) != 0 {
		t.Errorf("t1 Thread = %+v, want empty (no predecessor)", calls[0].Thread)
	}

	// t2: continues:t1 → Thread is exactly [t1 pair] (single entry).
	want2 := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}
	if len(calls[1].Thread) != 1 {
		t.Fatalf("t2 Thread len = %d, want 1; got %+v", len(calls[1].Thread), calls[1].Thread)
	}
	if calls[1].Thread[0] != want2[0] {
		t.Errorf("t2 Thread[0] = %+v, want %+v", calls[1].Thread[0], want2[0])
	}

	// t3: continues:t2 → Thread is [t1 pair, t2 pair] in ROOT→CURRENT order.
	// t1 must be Thread[0] (oldest/root) and t2 must be Thread[1] (immediate predecessor).
	// Do NOT reverse this assertion: root→current is the invariant.
	want3 := []agent.ThreadTurn{
		{User: "u1", Assistant: "a1"}, // t1 — root, oldest
		{User: "u2", Assistant: "a2"}, // t2 — immediate predecessor
	}
	if len(calls[2].Thread) != 2 {
		t.Fatalf("t3 Thread len = %d, want 2; got %+v", len(calls[2].Thread), calls[2].Thread)
	}
	if calls[2].Thread[0] != want3[0] {
		t.Errorf("t3 Thread[0] = %+v, want %+v (root turn t1 must be FIRST)", calls[2].Thread[0], want3[0])
	}
	if calls[2].Thread[1] != want3[1] {
		t.Errorf("t3 Thread[1] = %+v, want %+v (predecessor t2 must be SECOND)", calls[2].Thread[1], want3[1])
	}
}

func TestRunAgentStep_EvaluateContinuesPopulatesContextEvidence(t *testing.T) {
	const yaml = `workflow: evaluator-context-evidence
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: draft
    container: lab
    uses: awf/llm
    with: { model: m, prompt: draft }
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties: { k: { type: string } }
  - id: critique
    container: lab
    uses: awf/llm
    continues: draft
    with: { model: m, prompt: critique }
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties: { k: { type: string } }
  - gate:
      max_attempts: 1
      until: "{{ step.judge.ok }}"
      generate:
        - id: gen
          container: lab
          uses: awf/llm
          with: { model: m, prompt: gen }
          output_schema:
            type: object
            additionalProperties: false
            required: [k]
            properties: { k: { type: string } }
      evaluate:
        - id: judge
          container: lab
          uses: awf/llm
          continues: critique
          with: { model: m, prompt: judge }
          output_schema:
            type: object
            additionalProperties: false
            required: [ok]
            properties: { ok: { type: boolean } }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true, ContextEvidence: true}).
		Script(0, fake.Result{Output: map[string]any{"k": "draft"}, Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"}}).
		Script(1, fake.Result{Output: map[string]any{"k": "critique"}, Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"}}).
		Script(2, fake.Result{Output: map[string]any{"k": "gen"}, Transcript: agent.ThreadTurn{User: "u3", Assistant: "a3"}}).
		Script(3, fake.Result{Output: map[string]any{"ok": true}, Transcript: agent.ThreadTurn{User: "judge prompt", Assistant: "judge answer"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 4 {
		t.Fatalf("len(calls) = %d, want 4", len(calls))
	}
	if len(calls[3].Thread) != 0 {
		t.Fatalf("judge Thread = %+v, want empty", calls[3].Thread)
	}
	if calls[3].Feedback != nil {
		t.Fatalf("judge Feedback = %v, want nil", calls[3].Feedback)
	}
	want := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}, {User: "u2", Assistant: "a2"}}
	if !reflect.DeepEqual(calls[3].ContextEvidence, want) {
		t.Fatalf("judge ContextEvidence = %+v, want %+v", calls[3].ContextEvidence, want)
	}
}

func TestRunAgentStep_EvaluatorContextTargetTranscriptCommitted(t *testing.T) {
	const yaml = `workflow: evaluator-context-transcript
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: source
    container: lab
    uses: awf/llm
    with: { model: m, prompt: source }
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties: { k: { type: string } }
  - gate:
      max_attempts: 1
      until: "{{ step.judge.ok }}"
      generate:
        - id: gen
          container: lab
          uses: awf/llm
          with: { model: m, prompt: gen }
          output_schema:
            type: object
            additionalProperties: false
            required: [k]
            properties: { k: { type: string } }
      evaluate:
        - id: judge
          container: lab
          uses: awf/llm
          continues: source
          with: { model: m, prompt: judge }
          output_schema:
            type: object
            additionalProperties: false
            required: [ok]
            properties: { ok: { type: boolean } }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true, ContextEvidence: true}).
		Script(0, fake.Result{Output: map[string]any{"k": "source"}, Transcript: agent.ThreadTurn{User: "source user", Assistant: "source answer"}}).
		Script(1, fake.Result{Output: map[string]any{"k": "gen"}, Transcript: agent.ThreadTurn{User: "gen", Assistant: "draft"}}).
		Script(2, fake.Result{Output: map[string]any{"ok": true}, Transcript: agent.ThreadTurn{User: "judge", Assistant: "approved"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	transcriptRef := map[string]string{}
	for _, ev := range events {
		if ev.Type != engine.EventNodeCompleted {
			continue
		}
		var d engine.NodeCompletedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal node.completed: %v", err)
		}
		transcriptRef[ev.Path] = d.TranscriptRef
	}
	if transcriptRef["source"] == "" {
		t.Fatal("source TranscriptRef is empty, want committed transcript for evaluator context evidence")
	}
}

func TestRunAgentStep_EvaluatorContextMissingTranscriptIsMechanicalFailure(t *testing.T) {
	const yaml = `workflow: evaluator-context-missing-transcript
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: source
    container: lab
    uses: awf/llm
    with: { model: m, prompt: source }
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties: { k: { type: string } }
  - gate:
      max_attempts: 1
      until: "{{ step.judge.ok }}"
      generate:
        - id: gen
          container: lab
          uses: awf/llm
          with: { model: m, prompt: gen }
          output_schema:
            type: object
            additionalProperties: false
            required: [k]
            properties: { k: { type: string } }
      evaluate:
        - id: judge
          container: lab
          uses: awf/llm
          continues: source
          with: { model: m, prompt: judge }
          output_schema:
            type: object
            additionalProperties: false
            required: [ok]
            properties: { ok: { type: boolean } }
`
	ld := loadAgentSimpleDef(t, yaml)
	rs := engine.NewRunState("r1", "d", nil)
	rs.RecordCompleted("source", engine.NodeResult{Outcome: engine.OutcomeOK, Outputs: map[string]any{"k": "source"}})

	var reg agent.Registry
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true, ContextEvidence: true}).
		Script(0, fake.Result{Output: map[string]any{"k": "gen"}, Transcript: agent.ThreadTurn{User: "gen", Assistant: "draft"}}).
		Script(1, fake.Result{Output: map[string]any{"ok": true}, Transcript: agent.ThreadTurn{User: "judge", Assistant: "approved"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if err == nil {
		t.Fatal("engine.Run err = nil, want missing transcript error")
	}
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomePermanentFailure)
	}
}

// TestRunAgentStep_LeafTranscriptNotCommitted pins the participates invariant:
// only a turn that is continued-FROM (a "thread target") commits a transcript
// blob. A leaf turn — one that continues someone else but nobody continues IT —
// must NOT commit a transcript (its NodeCompletedData.TranscriptRef is empty).
//
// Chain: turn1 (target of turn2) → turn2 (leaf, continues turn1, no successor).
// Expected: turn1.TranscriptRef != "" ; turn2.TranscriptRef == "".
func TestRunAgentStep_LeafTranscriptNotCommitted(t *testing.T) {
	const yaml = `workflow: leaf-no-transcript
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: turn1
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p1"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
  - id: turn2
    container: lab
    uses: anthropic/claude-code
    continues: turn1
    with:
      prompt: "p2"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
		Script(0, fake.Result{
			Output:     map[string]any{"k": "v1"},
			Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"k": "v2"},
			Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	// Scan the committed node.completed events and check TranscriptRef presence.
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	transcriptRef := map[string]string{} // step path → TranscriptRef (empty means not committed)
	for _, ev := range events {
		if ev.Type != engine.EventNodeCompleted {
			continue
		}
		var d engine.NodeCompletedData
		if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
			t.Fatalf("unmarshal NodeCompletedData at %q: %v", ev.Path, uerr)
		}
		transcriptRef[ev.Path] = d.TranscriptRef
	}

	// turn1 is a thread target (turn2 continues it) → its transcript MUST be committed.
	if transcriptRef["turn1"] == "" {
		t.Errorf("turn1 (thread target): TranscriptRef is empty, want non-empty")
	}
	// turn2 is a leaf (no step continues it) → its transcript must NOT be committed.
	if transcriptRef["turn2"] != "" {
		t.Errorf("turn2 (leaf): TranscriptRef = %q, want empty (leaf turns must not commit transcript blobs)", transcriptRef["turn2"])
	}
}

// TestRunAgentStep_ContainerlessInputFilesResolve pins the feat/awf-llm-file-input
// Task 2 behavior: an agent step whose runtime omits container: (Container == "")
// declaring input_files is NO LONGER blanket-rejected with "input_files requires a
// container". Instead the interpreter resolves the refs to bytes for inline delivery.
// This case references an UNDECLARED producer (step.recon.files.report with no `recon`
// step in the graph), so resolution fails as a permanent_failure on the ref itself —
// proving the old container guard no longer short-circuits and the new resolver runs.
func TestRunAgentStep_ContainerlessInputFilesResolve(t *testing.T) {
	var reg agent.Registry
	// A containerless adapter — the only kind permitted to carry an empty
	// container: at run start (cli/runtimes.go). The fake's Containerless cap
	// lets the dispatcher's own guard pass; we are pinning the interpreter guard.
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"k": "v"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:         "hunt",
				Uses:       "awf/llm",
				With:       ir.RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	dispatcher := &engine.LocalDispatcher{Backend: container.NewFake(), Resolver: &reg}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (unresolvable input_files ref must permanent-fail)", oc, engine.OutcomePermanentFailure)
	}
	// The new resolver rejects the ref itself, NOT with the old container guard.
	if err == nil {
		t.Fatalf("err = nil, want a permanent_failure on the unresolvable input_files ref")
	}
	if strings.Contains(err.Error(), "input_files requires a container") {
		t.Errorf("err = %v, must NOT mention the removed 'input_files requires a container' guard", err)
	}
	// The adapter must never be launched — resolution fails before dispatch.
	if len(fk.Calls()) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (resolution must fail before Launch)", len(fk.Calls()))
	}
}

// TestRunAgentStep_StagesInputFiles pins the SP1 agent-step staging wiring
// (Task 7b): a CODE producer (certified named output_files) hands an artifact to
// a containerized AGENT consumer via input_files. The interpreter resolves the
// committed CAS ref, Blobs.Get's the bytes, and the dispatcher CopyTo's them into
// the agent's container BEFORE Launch. Mirrors the code-step cross-container test.
func TestRunAgentStep_StagesInputFiles(t *testing.T) {
	be := container.NewFake()
	labH, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	boxH, err := be.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"k": "v"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The code producer's exec produces /out/report.md (seeded onto lab's handle).
	sentinel := []byte("recon findings\n")
	be.ProgramExec("./recon.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := be.WriteFile(labH, "/out/report.md", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	disp := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": labH, "box": boxH},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}, "box": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "recon", Container: "lab", Run: "./recon.sh",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
			},
			&ir.AgentStep{
				ID: "hunt", Container: "box", Uses: "anthropic/claude-code",
				With:         ir.RawConfig{"prompt": "go"},
				OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []any{"k"}, "properties": map[string]any{"k": map[string]any{"type": "string"}}},
				InputFiles:   map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	if _, ok := rs.LookupCompleted("hunt"); !ok {
		t.Fatal("RunState.Completed missing 'hunt'")
	}
	// The sentinel was seeded ONLY into lab; hunt runs in box. The staged bytes
	// landing in box at /work/report.md proves resolve→Get→CopyTo-before-Launch.
	got, err := be.CaptureFiles(context.Background(), boxH, []string{"/work/report.md"})
	if err != nil {
		t.Fatalf("CaptureFiles box /work/report.md: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(sentinel) {
		t.Errorf("staged into box = %+v, want one file with content %q", got, sentinel)
	}
}

func TestRunAgentStep_OutputFilesPathTemplatedAndCommitted(t *testing.T) {
	const yaml = `workflow: agent-output-files
version: 1
input_schema:
  type: object
  additionalProperties: false
  required: [cve_id]
  properties:
    cve_id: { type: string }
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: write_record
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "write the record"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
    output_files:
      record: "/work/records/{{ input.cve_id }}.json"
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	if err := be.WriteFile(h, "/work/records/CVE-2026-0001.json", []byte(`{"cve":"CVE-2026-0001"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", map[string]any{"cve_id": "CVE-2026-0001"})

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.LookupCompleted("write_record")
	if !ok {
		t.Fatal("RunState.Completed missing write_record")
	}
	cas, ok := nr.Files["/work/records/CVE-2026-0001.json"]
	if !ok {
		t.Fatalf("Committed files = %+v, want substituted record path", nr.Files)
	}
	got, err := blobs.Get(cas)
	if err != nil {
		t.Fatalf("Blobs.Get: %v", err)
	}
	if string(got) != `{"cve":"CVE-2026-0001"}` {
		t.Errorf("Blob content = %q", got)
	}
}

// mustJSON is a per-package test helper. Task 2 defined an identical body
// in engine/events_test.go (package engine — internal); this file is
// package engine_test (external), a separate scope, so we declare it
// again here. Standard Go idiom for crossing the internal/external test
// boundary (cf. stdlib net/http's export_test.go vs http_test.go).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// capturingDispatcher wraps a base Dispatcher and records each NodeIntent
// it receives — the test inspects intent.ResolvedInputs.Feedback to verify
// runAgentStep's gate-repair feedback population (Task 9). Task 10 verifies
// the AgentInvocation.Feedback end-to-end via the fake adapter; this test
// scopes to runAgentStep's deliverable in isolation so it can be bisected
// to Task 9 without depending on Task 10's threading change in runAgent.
type capturingDispatcher struct {
	inner    engine.Dispatcher
	captured []engine.NodeIntent
}

func (c *capturingDispatcher) Run(ctx context.Context, intent engine.NodeIntent) (engine.DispatchResult, <-chan container.IOChunk, error) {
	c.captured = append(c.captured, intent)
	return c.inner.Run(ctx, intent)
}

func TestRunAgentStep_FeedbackPopulatedOnGateRepair(t *testing.T) {
	const yaml = `workflow: gate-agent-feedback
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      max_attempts: 2
      until: "{{ evaluate.verified }}"
      generate:
        - id: gen
          container: lab
          uses: anthropic/claude-code
          with:
            prompt: "write the thing"
          output_schema:
            type: object
            additionalProperties: false
            required: [done]
            properties:
              done: { type: boolean }
      evaluate:
        - id: judge
          container: lab
          uses: anthropic/claude-code
          with:
            prompt: "evaluate"
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	// Indices 0 (gen attempt 1) and 2 (gen attempt 2) — generator outputs.
	// Indices 1 (judge attempt 1) and 3 (judge attempt 2) — evaluator verdicts.
	// Same adapter ref handles both; fake's call index advances per Launch.
	fk := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"done": false}}).
		Script(1, fake.Result{Output: map[string]any{"verified": false, "feedback": "missing detection"}}).
		Script(2, fake.Result{Output: map[string]any{"done": true}}).
		Script(3, fake.Result{Output: map[string]any{"verified": true, "feedback": ""}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	base := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	cap := &capturingDispatcher{inner: base}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, cap, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	if len(cap.captured) != 4 {
		t.Fatalf("captured intents = %d, want 4 (2 generate + 2 evaluate)", len(cap.captured))
	}

	// Locate the generate-attempt-2 intent — its path contains
	// ".attempt-2.generate". That's the one runAgentStep must populate with
	// the prior verdict from attempt 1's evaluator.
	var gen2 *engine.NodeIntent
	var gen1 *engine.NodeIntent
	for i := range cap.captured {
		path := cap.captured[i].Path
		if !strings.Contains(path, ".generate") {
			continue
		}
		if strings.Contains(path, ".attempt-1.") {
			gen1 = &cap.captured[i]
		}
		if strings.Contains(path, ".attempt-2.") {
			gen2 = &cap.captured[i]
		}
	}
	if gen1 == nil {
		t.Fatal("generate attempt-1 intent not captured")
	}
	if gen2 == nil {
		t.Fatal("generate attempt-2 intent not captured")
	}

	// Attempt 1: no prior verdict → Feedback must be nil.
	if gen1.ResolvedInputs.Feedback != nil {
		t.Errorf("gen1.ResolvedInputs.Feedback = %v, want nil (attempt 1, no prior verdict)", gen1.ResolvedInputs.Feedback)
	}

	// Attempt 2: Feedback must carry attempt-1's evaluator verdict.
	if gen2.ResolvedInputs.Feedback == nil {
		t.Fatalf("gen2.ResolvedInputs.Feedback = nil, want populated with prior verdict")
	}
	if gen2.ResolvedInputs.Feedback["verified"] != false {
		t.Errorf("gen2.ResolvedInputs.Feedback[verified] = %v, want false", gen2.ResolvedInputs.Feedback["verified"])
	}
	if gen2.ResolvedInputs.Feedback["feedback"] != "missing detection" {
		t.Errorf("gen2.ResolvedInputs.Feedback[feedback] = %v, want %q", gen2.ResolvedInputs.Feedback["feedback"], "missing detection")
	}
}

func TestRunAgentStepIsGateEvaluateRejectsPersistentBeforeLaunch(t *testing.T) {
	wf := &ir.Workflow{
		ID:      "gate-persistent-evaluate",
		Version: 1,
		Containers: map[string]ir.Container{
			"lab": {Image: "oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		},
		Graph: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{
					&ir.CodeStep{ID: "gen", Container: "lab", Run: "true"},
				},
				Evaluate: ir.NodeList{
					&ir.AgentStep{
						ID:   "judge",
						Uses: "live/agent",
						OutputSchema: &ir.JSONSchema{
							"type":                 "object",
							"additionalProperties": false,
							"required":             []any{"verified"},
							"properties": map[string]any{
								"verified": map[string]any{"type": "boolean"},
							},
						},
					},
				},
				Until:       ir.Expr("{{ evaluate.verified }}"),
				MaxAttempts: 1,
			},
		},
	}
	ld := &ir.LoadedDefinition{Workflow: wf}

	var reg agent.Registry
	fk := fake.New("live/agent").
		WithCaps(agent.Caps{Containerless: true, PersistentSession: true}).
		Script(0, fake.Result{Output: map[string]any{"verified": true}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	be.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	dispatcher := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err: %v)", oc, engine.OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "PersistentSession") {
		t.Fatalf("err = %v, want PersistentSession rejection", err)
	}
	if calls := fk.Calls(); len(calls) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0 (evaluate guard must fire before Launch)", len(calls))
	}
}

type replayRequiredAdapter struct {
	ref        string
	launchErr  error
	outcomeErr error
	events     []agent.AgentEvent
	calls      int
}

func (a *replayRequiredAdapter) Ref() string { return a.ref }

func (*replayRequiredAdapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: true, Containerless: true, PersistentSession: true}
}

func (*replayRequiredAdapter) Version(context.Context, container.Handle) (string, error) {
	return "replay-test-v1", nil
}

func (*replayRequiredAdapter) ValidateConfig(ir.RawConfig) error { return nil }

func (a *replayRequiredAdapter) Launch(context.Context, container.Handle, agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	a.calls++
	if a.launchErr != nil {
		return nil, nil, a.launchErr
	}
	events := make(chan agent.AgentEvent, len(a.events))
	outcomes := make(chan agent.AgentOutcome, 1)
	for _, ev := range a.events {
		events <- ev
	}
	close(events)
	outcomes <- agent.AgentOutcome{Err: a.outcomeErr}
	close(outcomes)
	return events, outcomes, nil
}

type staticDispatcher struct {
	dr  engine.DispatchResult
	err error
}

func (d staticDispatcher) Run(context.Context, engine.NodeIntent) (engine.DispatchResult, <-chan container.IOChunk, error) {
	ch := make(chan container.IOChunk)
	close(ch)
	return d.dr, ch, d.err
}

func singleAgentDefinition() *ir.LoadedDefinition {
	return &ir.LoadedDefinition{
		Workflow: &ir.Workflow{
			ID:      "live-finalizer",
			Version: 1,
			Graph: ir.NodeList{
				&ir.AgentStep{ID: "live", Uses: "live/agent"},
			},
		},
	}
}

func countEvents(t *testing.T, log state.Log, typ, path string) int {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == typ && ev.Path == path {
			n++
		}
	}
	return n
}

func assertNoEventTypeAtPath(t *testing.T, log state.Log, typ, path string) {
	t.Helper()
	if got := countEvents(t, log, typ, path); got != 0 {
		t.Fatalf("%s count at %q = %d, want 0", typ, path, got)
	}
}

// TestRunAgentStep_SessionDirSetWhenPersistentSession verifies that the
// interpreter sets ResolvedInputs.SessionDir for a PersistentSession adapter —
// derived from Caps.StagingRoot + RunID alone (no SessionPathProvider, no cwd).
// The evidence is that node.completed carries a non-empty session_ref — which
// only happens if the dispatcher's ReadTreeAt(SessionDir) capture block fires,
// which only fires when SessionDir is set.
func TestRunAgentStep_SessionDirSetWhenPersistentSession(t *testing.T) {
	const wfYAML = `workflow: session-dir-wiring
version: 1
containers:
  ws:
    image: oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: gen
    container: ws
    uses: test/session-fake
    with:
      prompt: "hello"
`
	ld := loadAgentSimpleDef(t, wfYAML)

	fk := fake.New("test/session-fake").
		WithCaps(agent.Caps{NativeSchema: true, PersistentSession: true, IsolatedConfigDir: true}).
		Script(0, fake.Result{})

	// The engine derives SessionDir = <StagingRoot>/claude-session/<RunID>/projects.
	// The fake StagingRoot is /work/.awf and the RunID is "r1" (run.started below).
	const sessionDir = "/work/.awf/claude-session/r1/projects"
	f := container.NewFake()
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate claude writing a transcript under the per-run projects/ subtree so
	// the capture block (ReadTreeAt(SessionDir)) succeeds.
	if wErr := f.WriteFileAt(t.Context(), h, sessionDir+"/-work/gen.jsonl", []byte(`{"session":"r1"}`)); wErr != nil {
		t.Fatalf("seed transcript: %v", wErr)
	}

	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  f,
		Handles:  map[string]container.Handle{"ws": h},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"}),
	}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, runErr := engine.Run(t.Context(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{})
	if runErr != nil {
		t.Fatalf("engine.Run: %v", runErr)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	// Proof: node.completed carries a session_ref iff ReadTreeAt(SessionDir) was
	// called — which only happens when SessionDir was set on ResolvedInputs.
	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	var foundSessionRef bool
	for _, ev := range events {
		if ev.Type == engine.EventNodeCompleted && ev.Path == "gen" {
			var d engine.NodeCompletedData
			if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
				t.Fatalf("unmarshal NodeCompletedData: %v", uerr)
			}
			if d.SessionRef != "" {
				foundSessionRef = true
			}
		}
	}
	if !foundSessionRef {
		t.Fatal("node.completed for gen has no session_ref — SessionDir was not set by the interpreter (PersistentSession adapter)")
	}
}

// TestRunAgentStep_SessionDirNotSetForPlainAdapter verifies that a
// non-PersistentSession adapter leaves ResolvedInputs.SessionDir empty, so no
// session capture fires and node.completed carries no session_ref.
func TestRunAgentStep_SessionDirNotSetForPlainAdapter(t *testing.T) {
	const wfYAML = `workflow: plain-adapter-no-session
version: 1
containers:
  ws:
    image: oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: gen
    container: ws
    uses: test/plain-fake
    with:
      prompt: "hello"
`
	ld := loadAgentSimpleDef(t, wfYAML)

	fk := fake.New("test/plain-fake").
		WithCaps(agent.Caps{NativeSchema: true, PersistentSession: false}).
		Script(0, fake.Result{})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	f := container.NewFake()
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  f,
		Handles:  map[string]container.Handle{"ws": h},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"}),
	}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, runErr := engine.Run(t.Context(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{})
	if runErr != nil {
		t.Fatalf("engine.Run: %v", runErr)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	for _, ev := range events {
		if ev.Type == engine.EventNodeCompleted && ev.Path == "gen" {
			var d engine.NodeCompletedData
			if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
				t.Fatalf("unmarshal NodeCompletedData: %v", uerr)
			}
			if d.SessionRef != "" {
				t.Errorf("plain adapter: node.completed has session_ref %q, want empty (SessionDir must not be set for non-PersistentSession adapters)", d.SessionRef)
			}
		}
	}
}

// TestRunAgentStep_ConfigDirSetForIsolatedConfigButNoCapture verifies the base
// anthropic/claude-code path: a container-backed IsolatedConfigDir adapter that is
// NOT PersistentSession gets a per-run CLAUDE_CONFIG_DIR threaded via
// inv.SessionConfigDir (so concurrent runs don't collide on shared ~/.claude), but
// NO session subtree is captured (node.completed carries no session_ref).
func TestRunAgentStep_ConfigDirSetForIsolatedConfigButNoCapture(t *testing.T) {
	const wfYAML = `workflow: isolated-config-no-session
version: 1
containers:
  ws:
    image: oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: gen
    container: ws
    uses: test/isolated-fake
    with:
      prompt: "hello"
`
	ld := loadAgentSimpleDef(t, wfYAML)

	fk := fake.New("test/isolated-fake").
		WithCaps(agent.Caps{NativeSchema: true, IsolatedConfigDir: true}). // base claude: config dir, NOT PersistentSession
		Script(0, fake.Result{})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	f := container.NewFake()
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{Backend: f, Handles: map[string]container.Handle{"ws": h}, Resolver: &reg}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, runErr := engine.Run(t.Context(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{})
	if runErr != nil {
		t.Fatalf("engine.Run: %v", runErr)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	// The engine threaded a per-run CLAUDE_CONFIG_DIR (fake StagingRoot /work/.awf).
	calls := fk.Calls()
	if len(calls) == 0 {
		t.Fatal("fake adapter never launched")
	}
	if got, want := calls[0].SessionConfigDir, "/work/.awf/claude-session/r1"; got != want {
		t.Errorf("inv.SessionConfigDir = %q, want %q", got, want)
	}

	// ...but NO session subtree was captured (not PersistentSession).
	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	for _, ev := range events {
		if ev.Type == engine.EventNodeCompleted && ev.Path == "gen" {
			var d engine.NodeCompletedData
			if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
				t.Fatalf("unmarshal: %v", uerr)
			}
			if d.SessionRef != "" {
				t.Errorf("IsolatedConfigDir-only adapter has session_ref %q, want empty (no capture without PersistentSession)", d.SessionRef)
			}
		}
	}
}

// TestRunAgentStep_SessionDirNotSetForContainerlessSession is the regression test
// for the codexlive gate bug: subtree capture/restore reads a container filesystem,
// so it must be gated on PersistentSession AND !Containerless. A Containerless
// adapter (e.g. agent/codexlive) runs with a ZERO handle, so if SessionDir were set
// for it the capture block ReadTreeAt(ctx, h, SessionDir) would hit "unknown handle"
// and turn every successful run into a mechanical failure. This test pins that a
// Containerless+PersistentSession adapter (a) succeeds and (b) gets no session_ref.
// Against the un-narrowed gate (PersistentSession alone) it fails: the run does not
// reach OutcomeOK.
func TestRunAgentStep_SessionDirNotSetForContainerlessSession(t *testing.T) {
	var reg agent.Registry
	// codexlive-shaped: Containerless + PersistentSession (a live-process session,
	// NOT a config-dir subtree). Container-omitting step => zero handle at dispatch.
	fk := fake.New("test/live-session").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true, PersistentSession: true}).
		Script(0, fake.Result{})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "gen", Uses: "test/live-session", With: ir.RawConfig{"prompt": "hi"}},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	// Containerless dispatch: Backend present (non-nil) but NO Handles — the step
	// runs with a zero handle, exactly as codexlive does.
	dispatcher := &engine.LocalDispatcher{Backend: container.NewFake(), Resolver: &reg}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, runErr := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if runErr != nil {
		t.Fatalf("engine.Run: %v", runErr)
	}
	// The load-bearing assertion: a Containerless session adapter must succeed. Under
	// the un-narrowed gate, SessionDir is set and ReadTreeAt(zero handle) fails the run.
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Containerless PersistentSession adapter must not trigger subtree capture)", oc, engine.OutcomeOK)
	}
	// And it must have actually run (not failed before dispatch).
	if len(fk.Calls()) == 0 {
		t.Fatalf("fake was never launched — test did not exercise the dispatch/capture path")
	}
	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	for _, ev := range events {
		if ev.Type == engine.EventNodeCompleted && ev.Path == "gen" {
			var d engine.NodeCompletedData
			if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
				t.Fatalf("unmarshal NodeCompletedData: %v", uerr)
			}
			if d.SessionRef != "" {
				t.Errorf("containerless session adapter: node.completed has session_ref %q, want empty (SessionDir must not be set for Containerless adapters)", d.SessionRef)
			}
		}
	}
}

// autoTreeFake wraps *container.Fake and overrides ReadTreeAt to return a fixed
// session subtree for ANY dir/handle, so the per-run session capture block
// (Backend.ReadTreeAt(SessionDir)) succeeds for tests that use a PersistentSession
// fake adapter but do not seed a real session subtree (e.g. the live-event tests).
type autoTreeFake struct {
	*container.Fake
}

func (a *autoTreeFake) ReadTreeAt(_ context.Context, _ container.Handle, _ string) ([]byte, error) {
	return container.BuildTreeTar(map[string][]byte{"-work/s.jsonl": []byte("{}")})
}
