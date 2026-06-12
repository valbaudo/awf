package obs

import (
	"fmt"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

const (
	// contentInlineCap bounds an inlined content value — kept equal to
	// engine.AgentEventInlineThreshold so a payload the log inlines is never
	// truncated here (the OTel Go SDK does NOT cap attribute length). One source
	// via the engine const so they cannot drift.
	contentInlineCap = engine.AgentEventInlineThreshold
	// contentPreviewCap bounds the preview of blob-backed content (the full bytes
	// stay retrievable via the emitted CAS ref).
	contentPreviewCap = 256
)

// boundedString returns b as a string truncated to limit bytes, with a marker
// when truncated, so a multi-MB blob never becomes a multi-MB span attribute.
func boundedString(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + fmt.Sprintf("…[truncated %d bytes]", len(b)-limit)
}

// attachNodeCompletedContent attaches the typed-output and stdout blobs (when
// present) as attributes on s. Called only when CaptureContent is set.
func attachNodeCompletedContent(s *Span, d engine.NodeCompletedData, blobs state.Blobs) {
	if d.OutputsRef != "" {
		s.Attributes[AttrNodeOutputRef] = d.OutputsRef
		if b, gerr := blobs.Get(d.OutputsRef); gerr != nil {
			s.Attributes[AttrNodeOutputError] = gerr.Error()
		} else {
			s.Attributes[AttrNodeOutput] = boundedString(b, contentInlineCap)
		}
	}
	if d.StdoutRef != "" {
		s.Attributes[AttrNodeStdoutRef] = d.StdoutRef
		if b, gerr := blobs.Get(d.StdoutRef); gerr != nil {
			s.Attributes[AttrNodeStdoutError] = gerr.Error()
		} else {
			s.Attributes[AttrNodeStdout] = boundedString(b, contentInlineCap)
		}
	}
}

// attachAgentEventContent builds the attribute map for an agent.event span-event
// and appends it to s.Events. Called only when CaptureContent is set and the
// AgentEventData has already been unmarshalled.
func attachAgentEventContent(s *Span, d engine.AgentEventData, blobs state.Blobs, ts time.Time) {
	attrs := map[string]any{AttrAgentEventKind: d.Kind}
	if d.Stream != "" {
		attrs[AttrAgentEventStream] = d.Stream
	}
	if d.Live {
		attrs[AttrAgentEventLive] = true
		if d.DisplayClass != "" {
			attrs[AttrAgentEventDisplayClass] = d.DisplayClass
		}
		if d.DisplayTool != "" {
			attrs[AttrAgentEventDisplayTool] = agent.RedactDisplayText(agent.SanitizeDisplayText(d.DisplayTool))
		}
		if d.DisplaySummary != "" {
			attrs[AttrAgentEventDisplaySummary] = agent.RedactDisplayText(agent.SanitizeDisplayText(d.DisplaySummary))
		}
		if d.DisplayLines > 0 {
			attrs[AttrAgentEventDisplayLines] = int64(d.DisplayLines)
		}
		if d.DisplayBytes > 0 {
			attrs[AttrAgentEventDisplayBytes] = int64(d.DisplayBytes)
		}
		if d.DisplayIsError {
			attrs[AttrAgentEventDisplayIsError] = true
		}
	}
	switch {
	case d.PayloadInline != nil:
		// Inline payloads are below the log's 4 KiB offload threshold
		// (engine.AgentEventInlineThreshold); boundedString is defensive
		// against a corrupt/synthetic oversized inline payload, since the
		// OTel SDK does not cap attribute value length.
		attrs[AttrAgentEventPayload] = boundedAgentEventPayload(d.PayloadInline, contentInlineCap, d.Live)
	case d.PayloadRef != "":
		// Blob-backed (at or above 4 KiB): emit the CAS ref (full content stays
		// retrievable, §10) + a BOUNDED preview, never the whole blob.
		// Degrade on a missing/corrupt CONTENT blob — never abort the
		// trace (agent.event is non-authoritative; you trace damaged runs).
		attrs[AttrAgentEventPayloadRef] = d.PayloadRef
		if b, gerr := blobs.Get(d.PayloadRef); gerr != nil {
			attrs[AttrAgentEventPayloadError] = gerr.Error()
		} else {
			attrs[AttrAgentEventPayloadPreview] = boundedAgentEventPayload(b, contentPreviewCap, d.Live)
		}
	}
	s.Events = append(s.Events, SpanEvent{Name: EventNameAgentContent, Time: ts, Attributes: attrs})
}

func boundedAgentEventPayload(b []byte, limit int, live bool) string {
	if live {
		return boundedString([]byte(agent.RedactDisplayText(agent.SanitizeDisplayBytes(b))), limit)
	}
	return boundedString(b, limit)
}
