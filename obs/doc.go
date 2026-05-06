// Package obs is the read-only OpenTelemetry projection of the AWF event log.
//
// obs NEVER runs on the execution path (CLAUDE.md invariant: "everything else
// executes work or reads the log"). Project(events, blobs) is a pure,
// deterministic function from a folded log to a []Span tree; Export replays
// those spans into an OTel SpanExporter (stdout/file/OTLP). Attribute names are
// defined once in attrs.go (the swappable gen_ai.* mapping layer, pinned to OTel
// semconv spec v1.41.1).
package obs
