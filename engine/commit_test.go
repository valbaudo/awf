package engine_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestCommitHappyPath(t *testing.T) {
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()

	exit := 0
	dr := engine.DispatchResult{
		Outcome:  engine.OutcomeOK,
		ExitCode: &exit,
		Outputs:  map[string]any{"web_exploitable": true},
		Stdout:   []byte("done\n"),
		Files: []container.CapturedFile{
			{Path: "/out/report.json", Content: []byte(`{"k":"v"}`)},
		},
	}

	// Seed run.started so the log has a baseline event (matches the §8 invariant
	// that a log's first event is run.started; not strictly required by Commit
	// but matches realistic usage).
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}

	nr, err := engine.Commit(log, blobs, "triage", dr)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if nr.Outcome != engine.OutcomeOK {
		t.Errorf("nr.Outcome = %v, want ok", nr.Outcome)
	}
	if nr.OutputsRef == "" {
		t.Error("nr.OutputsRef is empty")
	}
	if nr.StdoutRef == "" {
		t.Error("nr.StdoutRef is empty")
	}
	if len(nr.Files) != 1 || nr.Files["/out/report.json"] == "" {
		t.Errorf("nr.Files = %v, want one entry with ref", nr.Files)
	}
	// Materialized fields preserved on the returned NodeResult so callers don't
	// re-Get from blobs.
	if !mapEqual(nr.Outputs, dr.Outputs) {
		t.Errorf("nr.Outputs = %v, want %v", nr.Outputs, dr.Outputs)
	}
	if string(nr.Stdout) != "done\n" {
		t.Errorf("nr.Stdout = %q, want %q", nr.Stdout, "done\n")
	}

	// Refs are reachable.
	out, err := blobs.Get(nr.OutputsRef)
	if err != nil {
		t.Errorf("Get(OutputsRef): %v", err)
	}
	if !strings.Contains(string(out), `"web_exploitable":true`) {
		t.Errorf("OutputsRef content = %q, want includes web_exploitable", out)
	}
	stdoutBlob, err := blobs.Get(nr.StdoutRef)
	if err != nil {
		t.Errorf("Get(StdoutRef): %v", err)
	}
	if string(stdoutBlob) != "done\n" {
		t.Errorf("StdoutRef content = %q, want %q", stdoutBlob, "done\n")
	}
	fileBlob, err := blobs.Get(nr.Files["/out/report.json"])
	if err != nil {
		t.Errorf("Get(file): %v", err)
	}
	if string(fileBlob) != `{"k":"v"}` {
		t.Errorf("file ref content = %q, want %q", fileBlob, `{"k":"v"}`)
	}

	// Fold the log and assert the node.completed event landed with correct refs.
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2 (run.started + node.completed)", len(events))
	}
	if events[1].Type != engine.EventNodeCompleted {
		t.Errorf("events[1].Type = %q, want %q", events[1].Type, engine.EventNodeCompleted)
	}
	if events[1].Path != "triage" {
		t.Errorf("events[1].Path = %q, want %q", events[1].Path, "triage")
	}
}

func TestCommitRejectsNonOkOutcome(t *testing.T) {
	// spec §8: only ok-steps commit. A non-ok DispatchResult into Commit is
	// a caller bug — return an error, don't append anything.
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()
	dr := engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure}
	_, err := engine.Commit(log, blobs, "x", dr)
	if err == nil {
		t.Fatal("Commit with non-ok outcome should error, got nil")
	}
	events, _ := log.Fold()
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (failed commit must not append)", len(events))
	}
}

func TestCommitAtomicityInvariant(t *testing.T) {
	// Revision #9: table-driven over every point at which Blobs.Put or
	// Log.Append can fail. The atomicity invariant — "no node.completed event
	// references a missing blob" — must hold in EVERY row. With this
	// DispatchResult (Outputs + Stdout + 2 Files), Commit makes 4 Blobs.Put
	// calls in order: [Outputs, Stdout, file1, file2]. Index N below targets
	// the (N+1)-th Put. failAppendAt targets the (N+1)-th Log.Append; the
	// seed run.started consumes call #0, so failAppendAt=1 fails the
	// node.completed append.
	cases := []struct {
		name         string
		failPutAt    int // -1 = no fault
		failAppendAt int // -1 = no fault
	}{
		{"OutputsPutFails", 0, -1},    // first Put (Outputs) fails — no orphans
		{"StdoutPutFails", 1, -1},     // Stdout Put fails — Outputs orphaned
		{"FirstFilePutFails", 2, -1},  // file1 Put fails — Outputs+Stdout orphaned
		{"SecondFilePutFails", 3, -1}, // file2 Put fails — Outputs+Stdout+file1 orphaned
		{"LogAppendFails", -1, 1},     // all 4 Puts succeed, Append fails — all 4 orphaned
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := state.NewInMemoryLog(clock.System{})
			blobs := state.NewInMemoryBlobs()
			if c.failPutAt >= 0 {
				blobs.FailPutAfterN(c.failPutAt)
			}
			if c.failAppendAt >= 0 {
				log.FailAppendAfterN(c.failAppendAt)
			}
			// Seed run.started so the log has a baseline event for fold checks.
			if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d"}`)}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			exit := 0
			dr := engine.DispatchResult{
				Outcome:  engine.OutcomeOK,
				ExitCode: &exit,
				Outputs:  map[string]any{"k": "v"},
				Stdout:   []byte("step output\n"),
				Files: []container.CapturedFile{
					{Path: "/out/a.json", Content: []byte(`{"a":1}`)},
					{Path: "/out/b.json", Content: []byte(`{"b":2}`)},
				},
			}
			_, err := engine.Commit(log, blobs, "x", dr)
			if err == nil {
				t.Fatal("Commit should have errored, got nil")
			}

			// The atomicity invariant: scan EVERY event in the log; for every
			// node.completed found, every ref it points at MUST resolve via
			// blobs.Get. With these fault injections, no node.completed should
			// exist at all — but the loop is the right shape for slice 2.6's
			// conformance bucket, which generalizes to multi-step runs.
			events, _ := log.Fold()
			for _, e := range events {
				if e.Type != engine.EventNodeCompleted {
					continue
				}
				var d engine.NodeCompletedData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatalf("unmarshal node.completed: %v", err)
				}
				for _, ref := range collectRefs(d) {
					if _, err := blobs.Get(ref); err != nil {
						t.Errorf("%s: node.completed at path=%q references missing blob %q: %v", c.name, e.Path, ref, err)
					}
				}
			}
		})
	}
}

// collectRefs returns every blob ref a node.completed event points at — the
// shape slice 2.6's conformance bucket-3 will reuse.
func collectRefs(d engine.NodeCompletedData) []string {
	var refs []string
	if d.OutputsRef != "" {
		refs = append(refs, d.OutputsRef)
	}
	if d.StdoutRef != "" {
		refs = append(refs, d.StdoutRef)
	}
	for _, r := range d.Files {
		refs = append(refs, r)
	}
	return refs
}

func TestCommitPersistsMetrics(t *testing.T) {
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()

	ms := &agent.MetricSet{Cost: agent.MetricCost{USD: 0.5, Source: agent.CostSourceReported}, Tokens: agent.MetricTokens{Input: 10, Output: 20}, Turns: 1}
	if _, err := engine.Commit(log, blobs, "triage", engine.DispatchResult{Outcome: engine.OutcomeOK, Outputs: map[string]any{"x": 1}, Metrics: ms}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	last := events[len(events)-1]
	var d engine.NodeCompletedData
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Metrics == nil || d.Metrics.Cost.USD != 0.5 {
		t.Fatalf("node.completed.Metrics = %+v, want cost 0.5", d.Metrics)
	}
}

func TestCommitCodeStepOmitsMetrics(t *testing.T) {
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()

	if _, err := engine.Commit(log, blobs, "build", engine.DispatchResult{Outcome: engine.OutcomeOK, Stdout: []byte("done")}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	last := events[len(events)-1]
	// Metrics omitted from JSON (omitempty) when nil.
	if bytes.Contains(last.Data, []byte("\"metrics\"")) {
		t.Errorf("code-step node.completed must omit metrics; got %s", last.Data)
	}
}

func mapEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
