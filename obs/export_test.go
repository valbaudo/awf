package obs

import (
	"bytes"
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// seqIDGen is a TEST-ONLY deterministic counter IDGenerator giving stable
// trace/span IDs for assertions. NOT in production (m3): real exporters use the
// SDK default random generator. (All-zero IDs like these would violate W3C
// Trace Context entropy if ever shipped — another reason it stays test-only.)
type seqIDGen struct{ n byte }

func newSeqIDGen() *seqIDGen { return &seqIDGen{} }

func (g *seqIDGen) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	g.n++
	return trace.TraceID{g.n}, trace.SpanID{g.n}
}

func (g *seqIDGen) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	g.n++
	return trace.SpanID{g.n}
}

func TestExportReplaysSpansToExporter(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	spans := []Span{
		{Path: "", Name: "run", Kind: "run", Start: t0, End: t0.Add(10 * time.Second), Status: StatusOK,
			Attributes: map[string]any{AttrRunID: "r1"}},
		{Path: "s1", ParentPath: "", Name: "s1", Kind: "code", Start: t0.Add(1 * time.Second), End: t0.Add(3 * time.Second), Status: StatusOK,
			Attributes: map[string]any{AttrNodePath: "s1", AttrExitCode: int64(0)}},
		{Path: "s2", ParentPath: "", Name: "s2", Kind: "agent", Start: t0.Add(4 * time.Second), End: t0.Add(6 * time.Second), Pending: true,
			Attributes: map[string]any{AttrNodePath: "s2", AttrNodeOutcome: outcomeIncomplete}},
	}

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(newSeqIDGen()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()
	if err := Export(context.Background(), spans, tp); err != nil {
		t.Fatalf("Export: %v", err)
	}
	got := exp.GetSpans() // read before Shutdown clears the in-memory exporter
	if len(got) != 3 {
		t.Fatalf("exported %d spans, want 3", len(got))
	}
	// Past timestamps must stick.
	var s1 *tracetest.SpanStub
	for i := range got {
		if got[i].Name == "s1" {
			s1 = &got[i]
		}
	}
	if s1 == nil {
		t.Fatal("no exported span named s1")
	}
	if !s1.StartTime.Equal(t0.Add(1 * time.Second)) {
		t.Errorf("s1 start = %v, want replayed +1s", s1.StartTime)
	}
}

func TestStdoutExporterWritesSpans(t *testing.T) {
	var buf bytes.Buffer
	tp, err := NewStdoutProvider(&buf)
	if err != nil {
		t.Fatalf("NewStdoutProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()
	t0 := time.Unix(1000, 0).UTC()
	spans := []Span{{Path: "s1", Name: "s1", Kind: "code", Start: t0, End: t0.Add(time.Second), Status: StatusOK,
		Attributes: map[string]any{AttrNodePath: "s1"}}}
	if err := Export(context.Background(), spans, tp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("s1")) {
		t.Errorf("stdout exporter wrote no span; got %q", buf.String())
	}
}

func TestNewOTLPProviderConstructs(t *testing.T) {
	tp, err := NewOTLPProvider(context.Background(), "localhost:4318")
	if err != nil {
		t.Fatalf("NewOTLPProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()
}
