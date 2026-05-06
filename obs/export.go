package obs

import (
	"context"
	"fmt"
	"io"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Export replays a projected span tree into the OTel SDK via tp's exporter.
// Parent-before-child order is guaranteed by sorting on path depth so a child's
// parent SpanContext already exists in ctxByPath. Past timestamps are honored
// (trace.WithTimestamp). Pending spans are ended too (OTel cannot export an
// open span) carrying their incomplete marker. tp's SpanProcessor should be a
// SimpleSpanProcessor for deterministic, synchronous, ordered export. Span
// NAMES are the low-cardinality s.Name (step id / scope kind), set at
// projection time; the unique path is in the awf.node.path attribute (R8).
//
// R7 — error surface: per-span exporter failures are NOT returned here.
// SimpleSpanProcessor.OnEnd routes an ExportSpans error to the OTel GLOBAL
// ErrorHandler (otel.Handle) — set otel.SetErrorHandler to capture them. Export
// returns only ForceFlush's error (and a SimpleSpanProcessor buffers nothing to
// flush). The default stdout / in-memory paths in 6.1 need no delivery
// guarantee; a delivery-guaranteed exporter wrapper is a later concern.
func Export(ctx context.Context, spans []Span, tp *sdktrace.TracerProvider) error {
	tr := tp.Tracer("github.com/valbaudo/awf/obs")

	ordered := make([]Span, len(spans))
	copy(ordered, spans)
	sort.SliceStable(ordered, func(i, j int) bool {
		return depth(ordered[i].Path) < depth(ordered[j].Path)
	})

	ctxByPath := map[string]context.Context{}
	for _, s := range ordered {
		parentCtx := ctx
		if pc, ok := ctxByPath[s.ParentPath]; ok && s.Path != "" {
			parentCtx = pc
		}
		spanCtx, span := tr.Start(parentCtx, s.Name, trace.WithTimestamp(s.Start))
		span.SetAttributes(toKeyValues(s.Attributes)...)
		switch s.Status {
		case StatusOK:
			span.SetStatus(codes.Ok, "")
		case StatusError:
			span.SetStatus(codes.Error, s.StatusMsg)
		}
		for _, ev := range s.Events {
			span.AddEvent(ev.Name, trace.WithTimestamp(ev.Time), trace.WithAttributes(toKeyValues(ev.Attributes)...))
		}
		span.End(trace.WithTimestamp(s.End))
		ctxByPath[s.Path] = spanCtx
	}
	if err := tp.ForceFlush(ctx); err != nil {
		return fmt.Errorf("obs.Export: flush: %w", err)
	}
	return nil
}

// NewStdoutProvider builds a TracerProvider that writes spans to w as JSON via
// a synchronous SimpleSpanProcessor — the zero-infra default for `awf trace`
// and the conformance round-trip target. Uses the SDK default ID generator
// (random); deterministic IDs are a test-only concern (m3), not a prod knob.
func NewStdoutProvider(w io.Writer) (*sdktrace.TracerProvider, error) {
	exp, err := stdouttrace.New(stdouttrace.WithWriter(w), stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, fmt.Errorf("obs.NewStdoutProvider: %w", err)
	}
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))), nil
}

// NewOTLPProvider builds a TracerProvider exporting to an OTLP/HTTP endpoint
// (e.g. a local collector / Jaeger / Honeycomb). endpoint is host:port (no
// scheme); WithInsecure for a plaintext local collector.
func NewOTLPProvider(ctx context.Context, endpoint string) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("obs.NewOTLPProvider: %w", err)
	}
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))), nil
}

// depth counts '.'-separated segments so parents export before children.
func depth(path string) int {
	if path == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			n++
		}
	}
	return n
}

func toKeyValues(attrs map[string]any) []attribute.KeyValue {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic attribute order
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, k := range keys {
		switch v := attrs[k].(type) {
		case string:
			out = append(out, attribute.String(k, v))
		case int64:
			out = append(out, attribute.Int64(k, v))
		case float64:
			out = append(out, attribute.Float64(k, v))
		case bool:
			out = append(out, attribute.Bool(k, v))
		}
	}
	return out
}
