package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// approvedSchema returns a JSONSchema accepting {approved: bool} objects with
// no additional properties — used across multiple signal_step tests.
func approvedSchema() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"approved"},
		"properties":           map[string]any{"approved": map[string]any{"type": "boolean"}},
	}
}

func TestRunSignalStep_NilBrokerErrors(t *testing.T) {
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	ss := &ir.SignalStep{ID: "approve", Await: "human_review"}
	_, err := runSignalStep(context.Background(), ss, "approve", nil, rs, nil, log, blobs, &clock.Fake{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nil *signal.Broker") {
		t.Errorf("err = %v, want 'nil *signal.Broker'", err)
	}
}

func TestRunSignalStep_ResumeSkipsCompleted(t *testing.T) {
	rs := NewRunState("r", "d", nil)
	rs.RecordCompleted("approve", NodeResult{Outcome: OutcomeOK})
	ss := &ir.SignalStep{ID: "approve", Await: "human_review"}
	oc, err := runSignalStep(context.Background(), ss, "approve", nil, rs, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
}

func TestRunSignalStep_DeliversAndCommits(t *testing.T) {
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	if _, err := b.WriteSignal("human_review", []byte(`{"approved":true}`)); err != nil {
		t.Fatalf("WriteSignal: %v", err)
	}
	ss := &ir.SignalStep{
		ID:           "approve",
		Await:        "human_review",
		OutputSchema: approvedSchema(),
	}
	oc, err := runSignalStep(context.Background(), ss, "approve", nil, rs, nil, log, blobs, &clock.Fake{}, nil, b)
	if err != nil {
		t.Fatalf("runSignalStep: %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	// node.completed + signal.received in log. M11: verify PayloadRef on
	// signal.received MATCHES OutputsRef on node.completed — both must reference
	// the same CAS blob (the canonicalized payload).
	events, _ := log.Fold()
	var sigPayloadRef, completedOutputsRef string
	for _, e := range events {
		if e.Type == EventSignalReceived {
			var d SignalReceivedData
			_ = json.Unmarshal(e.Data, &d)
			sigPayloadRef = d.PayloadRef
		}
		if e.Type == EventNodeCompleted {
			var d NodeCompletedData
			_ = json.Unmarshal(e.Data, &d)
			completedOutputsRef = d.OutputsRef
		}
	}
	if sigPayloadRef == "" {
		t.Errorf("no signal.received event with PayloadRef")
	}
	if completedOutputsRef == "" {
		t.Errorf("no node.completed event with OutputsRef")
	}
	if sigPayloadRef != completedOutputsRef {
		t.Errorf("PayloadRef (%q) != OutputsRef (%q); both must reference the canonical payload blob", sigPayloadRef, completedOutputsRef)
	}
	// RunState.Completed populated.
	nr, ok := rs.LookupCompleted("approve")
	if !ok {
		t.Fatal("Completed[approve] not set")
	}
	if nr.Outputs["approved"] != true {
		t.Errorf("Outputs[approved] = %v, want true", nr.Outputs["approved"])
	}
}

func TestRunSignalStep_TimeoutIsRetryable(t *testing.T) {
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	timeout := ir.Duration(10 * time.Millisecond)
	ss := &ir.SignalStep{ID: "approve", Await: "human_review", Timeout: &timeout}
	oc, err := runSignalStep(context.Background(), ss, "approve", nil, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err == nil {
		t.Fatal("err = nil, want timeout")
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome = %q, want retryable_failure", oc)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want 'timeout'", err)
	}
}

func TestRunSignalStep_SchemaFailureIsRetryable(t *testing.T) {
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	if _, err := b.WriteSignal("name", []byte(`{"approved":"not-a-bool"}`)); err != nil {
		t.Fatal(err)
	}
	ss := &ir.SignalStep{
		ID:           "approve",
		Await:        "name",
		OutputSchema: approvedSchema(),
	}
	oc, err := runSignalStep(context.Background(), ss, "approve", nil, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err == nil {
		t.Fatal("err = nil, want schema-violation")
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome = %q, want retryable_failure", oc)
	}
	if !strings.Contains(err.Error(), "output_schema") {
		t.Errorf("err = %v, want 'output_schema'", err)
	}
}

func TestRunSignalStep_NoPayloadNoSchema(t *testing.T) {
	// Await with no output_schema + no payload → committed ok with empty outputs.
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	if _, err := b.WriteSignal("tick", nil); err != nil {
		t.Fatal(err)
	}
	ss := &ir.SignalStep{ID: "tick", Await: "tick"}
	oc, err := runSignalStep(context.Background(), ss, "tick", nil, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err != nil {
		t.Fatalf("runSignalStep: %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	nr, _ := rs.LookupCompleted("tick")
	if len(nr.Outputs) != 0 {
		t.Errorf("Outputs = %+v, want nil/empty", nr.Outputs)
	}
}

func TestRunSignalStep_HalfCommitResume(t *testing.T) {
	// Half-commit resume (Design Q7 + C6 refinement): a prior run committed
	// signal.received but crashed BEFORE node.completed. Fold populated
	// SignalReceivedAt[path] from the journaled signal.received event (REFS
	// ONLY — no Payload field). runSignalStep MUST NOT call broker.Receive
	// (no signal file exists; would block forever) AND MUST NOT re-append
	// signal.received (would duplicate the existing log entry). It re-derives
	// typed outputs via Blobs.Get + ValidateAgainstSchema.
	rs := NewRunState("r", "d", nil)
	blobs := state.NewInMemoryBlobs()
	// M14 fix: Blobs.Put real bytes (canonical JSON) and use the real ref —
	// test mock state stays internally consistent.
	canonicalBytes, _ := json.Marshal(map[string]any{"approved": true})
	payloadRef, err := blobs.Put(canonicalBytes)
	if err != nil {
		t.Fatalf("Put canonical payload: %v", err)
	}
	// Simulate post-Fold state: SignalReceivedAt populated; Completed not.
	rs.RecordSignalReceivedAt("approve", SignalReceivedEntry{
		Seq:        1,
		PayloadRef: payloadRef,
	})
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	// Broker has NO signal — would block forever if Receive were called.
	b := tempBroker(t)
	ss := &ir.SignalStep{
		ID:           "approve",
		Await:        "human_review",
		OutputSchema: approvedSchema(),
	}

	// Tight timeout — if Receive is called, this would fire and the test fails.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	oc, err := runSignalStep(ctx, ss, "approve", nil, rs, nil, log, blobs, &clock.Fake{}, nil, b)
	if err != nil {
		t.Fatalf("runSignalStep: %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("Broker.Receive was called (ctx timed out); half-commit path should skip it")
	}
	// Verify the log contains ONLY node.completed (NOT a duplicate signal.received).
	events, _ := log.Fold()
	var sigReceived, nodeCompleted int
	var completedOutputsRef string
	for _, e := range events {
		switch e.Type {
		case EventSignalReceived:
			sigReceived++
		case EventNodeCompleted:
			nodeCompleted++
			var d NodeCompletedData
			_ = json.Unmarshal(e.Data, &d)
			completedOutputsRef = d.OutputsRef
		}
	}
	if sigReceived != 0 {
		t.Errorf("signal.received events = %d, want 0 (half-commit must not re-append)", sigReceived)
	}
	if nodeCompleted != 1 {
		t.Errorf("node.completed events = %d, want 1", nodeCompleted)
	}
	// M11: node.completed.OutputsRef MUST match the original PayloadRef.
	if completedOutputsRef != payloadRef {
		t.Errorf("node.completed.OutputsRef = %q, want %q (SignalReceivedEntry.PayloadRef)", completedOutputsRef, payloadRef)
	}
	// Verify RunState.Completed populated with the re-derived payload.
	nr, ok := rs.LookupCompleted("approve")
	if !ok {
		t.Fatal("Completed[approve] not set")
	}
	if nr.Outputs["approved"] != true {
		t.Errorf("Outputs[approved] = %v, want true (re-derived via Blobs.Get + ValidateAgainstSchema)", nr.Outputs["approved"])
	}
	if nr.OutputsRef != payloadRef {
		t.Errorf("OutputsRef = %q, want %q (from SignalReceivedAt)", nr.OutputsRef, payloadRef)
	}
}

func TestRunSignalStep_CtxCancelledByPollerSkipsFailStep(t *testing.T) {
	// M7 fix: when pollControls cancels root ctx + sets runstate.IsCancelled(),
	// the handler returns ctx.Err() WITHOUT emitting node.failed (which would
	// be a redundant terminal record + block resume on the node.failed refusal).
	rs := NewRunState("r", "d", nil)
	rs.SetCancelled(true) // simulate poller having fired
	rs.SetCancelReason("operator cancel")
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	b := tempBroker(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate ctx already cancelled by poller

	ss := &ir.SignalStep{ID: "approve", Await: "human_review"}
	oc, err := runSignalStep(ctx, ss, "approve", nil, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if oc != "" {
		t.Errorf("outcome = %q, want \"\" (ctx-cancel during poller-cancel does NOT emit failStep)", oc)
	}
	// Verify NO node.failed event was emitted.
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == EventNodeFailed {
			t.Errorf("node.failed emitted despite poller cancel; want NO node.failed")
		}
	}
}
