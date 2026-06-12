package obs

import (
	"bytes"
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestCaptureContentAttachesAgentEventAndOutputs(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs, err := state.OpenBlobs(t.TempDir()) // FSBlobs — the guaranteed store
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	outRef, err := blobs.Put([]byte(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatalf("Put outputs: %v", err)
	}

	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1"}),
		ev(t, engine.EventNodeStarted, "a1", t0.Add(time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(2*time.Second), engine.AgentEventData{
			Kind: "assistant", Size: 5, PayloadInline: []byte("hello"),
		}),
		ev(t, engine.EventNodeCompleted, "a1", t0.Add(3*time.Second), engine.NodeCompletedData{
			Outcome: "ok", OutputsRef: outRef,
		}),
		ev(t, engine.EventRunFinished, "", t0.Add(4*time.Second), engine.RunFinishedData{Outcome: "ok"}),
	}

	// Capture OFF: no content attached.
	off, err := ProjectWithOptions(events, blobs, ProjectOptions{})
	if err != nil {
		t.Fatalf("Project off: %v", err)
	}
	s, _ := findSpan(off, "a1")
	if len(s.Events) != 0 || s.Attributes["awf.node.output"] != nil {
		t.Errorf("capture-off attached content: events=%d output=%v", len(s.Events), s.Attributes["awf.node.output"])
	}

	// Capture ON: inline agent.event → span event; typed output → bounded value + ref.
	on, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("Project on: %v", err)
	}
	s, _ = findSpan(on, "a1")
	found := false
	for _, e := range s.Events {
		if e.Name == "awf.agent.event" && e.Attributes["awf.agent.event.payload"] == "hello" && e.Attributes["awf.agent.event.kind"] == "assistant" {
			found = true
		}
	}
	if !found {
		t.Errorf("capture-on missing agent.event content: %+v", s.Events)
	}
	if s.Attributes["awf.node.output"] != `{"verdict":"pass"}` {
		t.Errorf("capture-on missing awf.node.output: %v", s.Attributes["awf.node.output"])
	}
	if s.Attributes["awf.node.output_ref"] != outRef {
		t.Errorf("capture-on must also emit the CAS ref: %v", s.Attributes["awf.node.output_ref"])
	}
}

func TestCaptureContentBoundsBlobBackedPayload(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs, err := state.OpenBlobs(t.TempDir())
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	big := bytes.Repeat([]byte("x"), 50_000) // > the 4 KiB log offload threshold
	ref, err := blobs.Put(big)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "a1", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(time.Second), engine.AgentEventData{Kind: "assistant", Size: len(big), PayloadRef: ref}),
		ev(t, engine.EventNodeCompleted, "a1", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	spans, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	s, _ := findSpan(spans, "a1")
	var ev0 SpanEvent
	for _, e := range s.Events {
		if e.Name == "awf.agent.event" {
			ev0 = e
		}
	}
	if ev0.Attributes["awf.agent.event.payload_ref"] != ref {
		t.Errorf("blob-backed payload must emit the CAS ref, got %v", ev0.Attributes["awf.agent.event.payload_ref"])
	}
	preview, _ := ev0.Attributes["awf.agent.event.payload_preview"].(string)
	// Bounded to contentPreviewCap (+ a small truncation marker), NOT the full 50 KB.
	// Coupled to the constant so raising contentPreviewCap can't silently slip through.
	if len(preview) > contentPreviewCap+64 {
		t.Errorf("preview not bounded: %d bytes (cap %d)", len(preview), contentPreviewCap)
	}
	if ev0.Attributes["awf.agent.event.payload"] != nil {
		t.Errorf("blob-backed payload must NOT be inlined under .payload: %v", ev0.Attributes["awf.agent.event.payload"])
	}
}

func TestTracePreviewRedactsLiveAgentPayload(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "a1", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(time.Second), engine.AgentEventData{
			Kind: "assistant", Live: true, Size: 18, PayloadInline: []byte("hi sk-liveSECRET123 \x1b]52;c;SECRET\a there\x00"),
		}),
	}
	spans, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	s, _ := findSpan(spans, "a1")
	var ev0 SpanEvent
	for _, e := range s.Events {
		if e.Name == "awf.agent.event" {
			ev0 = e
		}
	}
	if got := ev0.Attributes["awf.agent.event.payload"]; got != "hi sk-[redacted]  there" {
		t.Fatalf("live payload attr = %q, want sanitized text", got)
	}
}

func TestCaptureContentAttachesLiveDisplayMetadata(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "a1", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(time.Second), engine.AgentEventData{
			Kind: "tool_result", Stream: "stdout", Live: true, Size: 12,
			DisplayClass:   "tool_result",
			DisplayTool:    "shell\x1b[31m",
			DisplaySummary: "failed sk-liveSECRET123",
			DisplayLines:   3,
			DisplayBytes:   42,
			DisplayIsError: true,
			PayloadInline:  []byte("redacted"),
		}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(2*time.Second), engine.AgentEventData{
			Kind: "notice", Stream: "stderr", Live: true, Size: 6,
			DisplayClass: "notice", DisplaySummary: "lease finalizer needs cleanup",
			PayloadInline: []byte("notice"),
		}),
	}

	spans, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	s, _ := findSpan(spans, "a1")
	if len(s.Events) != 2 {
		t.Fatalf("span events = %d, want 2: %+v", len(s.Events), s.Events)
	}
	tool := s.Events[0].Attributes
	if tool[AttrAgentEventLive] != true ||
		tool[AttrAgentEventDisplayClass] != "tool_result" ||
		tool[AttrAgentEventDisplayTool] != "shell" ||
		tool[AttrAgentEventDisplaySummary] != "failed sk-[redacted]" ||
		tool[AttrAgentEventDisplayLines] != int64(3) ||
		tool[AttrAgentEventDisplayBytes] != int64(42) ||
		tool[AttrAgentEventDisplayIsError] != true {
		t.Fatalf("tool display attrs = %+v", tool)
	}
	notice := s.Events[1].Attributes
	if notice[AttrAgentEventDisplayClass] != "notice" ||
		notice[AttrAgentEventDisplaySummary] != "lease finalizer needs cleanup" {
		t.Fatalf("notice display attrs = %+v", notice)
	}
}

func TestCaptureContentDegradesOnMissingAgentEventBlob(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs, err := state.OpenBlobs(t.TempDir())
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	missing := state.RefFor([]byte("never stored")) // a valid ref never Put
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "a1", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventAgentEvent, "a1", t0.Add(time.Second), engine.AgentEventData{Kind: "assistant", Size: 12, PayloadRef: missing}),
		ev(t, engine.EventNodeCompleted, "a1", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	// A missing agent.event content blob must degrade (error marker), not abort.
	spans, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("capture must not abort on a missing agent.event blob: %v", err)
	}
	s, _ := findSpan(spans, "a1")
	var ev0 SpanEvent
	for _, e := range s.Events {
		if e.Name == "awf.agent.event" {
			ev0 = e
		}
	}
	if ev0.Attributes["awf.agent.event.payload_error"] == nil {
		t.Errorf("missing agent.event blob must attach payload_error: %+v", ev0.Attributes)
	}
	if ev0.Attributes["awf.agent.event.payload_ref"] != missing {
		t.Errorf("missing agent.event blob must still emit the ref: %v", ev0.Attributes["awf.agent.event.payload_ref"])
	}
	if ev0.Attributes["awf.agent.event.payload_preview"] != nil {
		t.Errorf("missing agent.event blob must NOT emit a preview: %v", ev0.Attributes["awf.agent.event.payload_preview"])
	}
}

func TestCaptureContentDegradesOnMissingBlob(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	blobs, err := state.OpenBlobs(t.TempDir())
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	missing := state.RefFor([]byte("never stored")) // a valid ref never Put
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "a1", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "a1", t0.Add(time.Second), engine.NodeCompletedData{Outcome: "ok", OutputsRef: missing}),
	}
	// A missing CONTENT blob must NOT abort the trace (observability degrades).
	spans, err := ProjectWithOptions(events, blobs, ProjectOptions{CaptureContent: true})
	if err != nil {
		t.Fatalf("capture must not abort on a missing content blob: %v", err)
	}
	s, _ := findSpan(spans, "a1")
	if s.Attributes["awf.node.output_error"] == nil {
		t.Errorf("missing-blob capture must attach awf.node.output_error: %+v", s.Attributes)
	}
	if s.Attributes["awf.node.output_ref"] != missing {
		t.Errorf("missing-blob capture must still emit the ref: %v", s.Attributes["awf.node.output_ref"])
	}
}

func TestContentInlineCapTracksEngineThreshold(t *testing.T) {
	if contentInlineCap != engine.AgentEventInlineThreshold {
		t.Fatalf("contentInlineCap %d != engine.AgentEventInlineThreshold %d — an inlined payload could be truncated in the trace", contentInlineCap, engine.AgentEventInlineThreshold)
	}
}
