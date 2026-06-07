package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/runlock"
	"github.com/valbaudo/awf/state"
)

// fastPoll shortens the SSE poll interval for tests and restores it after.
func fastPoll(t *testing.T) {
	t.Helper()
	prev := ssePollInterval
	ssePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { ssePollInterval = prev })
}

// readSSEProjection reads the next `data:` frame and decodes it as a Projection.
func readSSEProjection(t *testing.T, br *bufio.Reader) graph.Projection {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var p graph.Projection
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				t.Fatalf("decode SSE data: %v", err)
			}
			return p
		}
	}
}

// TestSSEInitialThenUpdate: connecting yields the current projection immediately, and a
// log change pushes an updated one. This is the live-overlay contract (the thing
// RunState alone could not provide, and the snapshot-only 2a could not stream).
func TestSSEInitialThenUpdate(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
	)
	ts := newTestServer(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?run=r1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)

	if got := readSSEProjection(t, br).RunOverlay["build"].State; got != "running" {
		t.Fatalf("initial SSE build=%q, want running", got)
	}

	// Mutate the run (build completes) -> the watch loop must push an update.
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
	)
	if got := readSSEProjection(t, br).RunOverlay["build"].State; got != "completed" {
		t.Errorf("after change SSE build=%q, want completed", got)
	}
}

// TestSSEStopsOnDisconnect: cancelling the request ends the stream promptly. The watch
// loop runs in the handler goroutine, so the handler returning IS the goroutine exiting
// -- there is nothing left to leak.
func TestSSEStopsOnDisconnect(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
	)
	ts := newTestServer(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?run=r1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	readSSEProjection(t, br) // initial frame

	cancel() // simulate client disconnect
	done := make(chan struct{})
	go func() {
		_, _ = br.ReadString('\n') // returns once the server-side handler stops writing
		_ = resp.Body.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after client disconnect (handler/goroutine leak)")
	}
}

func TestSSEUnknownRun404(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	resp, err := http.Get(ts.URL + "/api/events?run=ghost")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run SSE -> %d, want 404", resp.StatusCode)
	}
}

// TestRunsLiveness: an incomplete run with a held lock is "running"; without a holder it
// is "crashed". Exercises the shared runlock probe via /api/runs.
func TestRunsLiveness(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"live", "dead"} {
		writeRunLog(t, dir, id,
			state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: id, WorkflowDigest: testDigest})},
			state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		)
	}
	lk, err := runlock.Acquire(filepath.Join(dir, "runs", "live"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	ts := newTestServer(t, dir)
	resp, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct{ Runs []RunRow }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range body.Runs {
		got[r.RunID] = r.Status
	}
	if got["live"] != "running" {
		t.Errorf("live run status = %q, want running", got["live"])
	}
	if got["dead"] != "crashed" {
		t.Errorf("dead run status = %q, want crashed", got["dead"])
	}
}
