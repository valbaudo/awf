package obs

import (
	"fmt"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

const (
	// contentInlineCap bounds an inlined content value. Mirrors the log's
	// agentEventInlineThreshold (engine, 4096) — beyond this, content lives in a
	// blob and is referenced, not inlined verbatim (the OTel Go SDK does NOT cap
	// attribute value length by default, so obs must bound it).
	contentInlineCap = 4096
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
	switch {
	case d.PayloadInline != nil:
		// Inline payloads are already ≤ the log's 4 KiB offload threshold
		// (engine.agentEventInlineThreshold); boundedString is defensive
		// against a corrupt/synthetic oversized inline payload, since the
		// OTel SDK does not cap attribute value length.
		attrs[AttrAgentEventPayload] = boundedString(d.PayloadInline, contentInlineCap)
	case d.PayloadRef != "":
		// Blob-backed (>4 KiB): emit the CAS ref (full content stays
		// retrievable, §10) + a BOUNDED preview, never the whole blob.
		// Degrade on a missing/corrupt CONTENT blob — never abort the
		// trace (agent.event is non-authoritative; you trace damaged runs).
		attrs[AttrAgentEventPayloadRef] = d.PayloadRef
		if b, gerr := blobs.Get(d.PayloadRef); gerr != nil {
			attrs[AttrAgentEventPayloadError] = gerr.Error()
		} else {
			attrs[AttrAgentEventPayloadPreview] = boundedString(b, contentPreviewCap)
		}
	}
	s.Events = append(s.Events, SpanEvent{Name: EventNameAgentContent, Time: ts, Attributes: attrs})
}
