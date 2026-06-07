package ui

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

const testDigest = "awf-d1:sha256:test"

func demoWorkflow() *ir.Workflow {
	return &ir.Workflow{
		ID: "demo",
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "build", Run: "make"},
			&ir.Gate{
				Generate:    ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "x"}},
				Evaluate:    ir.NodeList{&ir.CodeStep{ID: "check", Run: "t"}},
				Until:       "ok",
				MaxAttempts: 2,
			},
		},
	}
}

func mustData(v any) []byte { b, _ := json.Marshal(v); return b }

// writeRunLog crafts <stateDir>/runs/<id>/log (mirrors cli/ls_test.go's helper; the cli
// one is package-private so ui re-declares its own).
func writeRunLog(t *testing.T, stateDir, id string, events ...state.Event) {
	t.Helper()
	runDir := filepath.Join(stateDir, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := state.OpenLog(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := lg.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
}

func newTestServer(t *testing.T, stateDir string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(demoWorkflow(), testDigest, stateDir).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getProjection(t *testing.T, url string) graph.Projection {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("GET %s -> %d: %s", url, r.StatusCode, b)
	}
	var p graph.Projection
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGraphStatic(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	p := getProjection(t, ts.URL+"/api/graph")
	if p.SchemaVersion != graph.SchemaVersion || p.Workflow != "demo" {
		t.Errorf("bad header: %+v", p)
	}
	if p.RunOverlay != nil {
		t.Errorf("static graph must not carry run_overlay: %+v", p.RunOverlay)
	}
	if len(p.Nodes) == 0 {
		t.Error("no nodes")
	}
}

func TestGraphSnapshotOverlay(t *testing.T) {
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	ts := newTestServer(t, dir)
	p := getProjection(t, ts.URL+"/api/graph?run=r1")
	st, ok := p.RunOverlay["build"]
	if !ok || st.State != "completed" {
		t.Errorf("overlay[build] = %+v, want completed", p.RunOverlay)
	}
}

func TestGraphUnknownRun404(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	r, err := http.Get(ts.URL + "/api/graph?run=ghost")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run -> %d, want 404", r.StatusCode)
	}
}

// TestGraphCacheReflectsChange: a second fetch after appending to the log must reflect
// the new state (the (size,mtime) cache key invalidates on change).
func TestGraphCacheReflectsChange(t *testing.T) {
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
	)
	ts := newTestServer(t, dir)
	if got := getProjection(t, ts.URL+"/api/graph?run=r1").RunOverlay["build"].State; got != "running" {
		t.Fatalf("first fetch build=%q, want running", got)
	}
	// Re-open the log and append a completion (changes size+mtime).
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
	)
	if got := getProjection(t, ts.URL+"/api/graph?run=r1").RunOverlay["build"].State; got != "completed" {
		t.Errorf("after append build=%q, want completed (cache should have invalidated)", got)
	}
}

func TestRunsDigestFilter(t *testing.T) {
	dir := t.TempDir()
	writeRunLog(t, dir, "mine",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "mine", WorkflowDigest: testDigest, WorkflowID: "demo"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	writeRunLog(t, dir, "other",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "other", WorkflowDigest: "different-digest", WorkflowID: "x"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	ts := newTestServer(t, dir)
	r, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	var body struct{ Runs []RunRow }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 || body.Runs[0].RunID != "mine" {
		t.Errorf("runs = %+v, want only 'mine' (digest-filtered)", body.Runs)
	}
}

func TestRunsEmpty(t *testing.T) {
	ts := newTestServer(t, t.TempDir()) // no runs/ dir
	r, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("empty runs -> %d, want 200", r.StatusCode)
	}
	var body struct{ Runs []RunRow }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 0 {
		t.Errorf("want empty runs, got %+v", body.Runs)
	}
}

// TestServeIndex proves the //go:embed dist wiring: GET / serves the SPA index.html.
func TestServeIndex(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	r, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(b), `id="root"`) {
		t.Errorf("GET / -> %d, body lacks #root (embed broken?): %.120s", r.StatusCode, b)
	}
}

func TestListenLoopback(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("listener bound to %q, want loopback", host)
	}
}

// TestConcurrentGraph exercises the cache mutex under -race.
func TestConcurrentGraph(t *testing.T) {
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
	)
	srv := New(demoWorkflow(), testDigest, dir)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := srv.projectionFor("r1"); err != nil {
				t.Errorf("projectionFor: %v", err)
			}
		}()
	}
	wg.Wait()
}
