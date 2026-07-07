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

// whereSchema accepts {candidate_id: string} objects — used by the keyed-await
// (where:) tests so the matched payload validates and commits.
func whereSchema() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"candidate_id"},
		"properties":           map[string]any{"candidate_id": map[string]any{"type": "string"}},
	}
}

func TestRunSignalStep_WhereMatchesByPayload(t *testing.T) {
	// Two buffered signals; the where: clause selects seq 2 (signal.candidate_id
	// == "b"). The non-matching seq 1 must stay buffered for another await.
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"a"}`)); err != nil {
		t.Fatalf("WriteSignal a: %v", err)
	}
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"b"}`)); err != nil {
		t.Fatalf("WriteSignal b: %v", err)
	}
	ss := &ir.SignalStep{
		ID:           "wait_oob",
		Await:        "oob",
		Where:        `{{ signal.candidate_id == "b" }}`,
		OutputSchema: whereSchema(),
	}
	// wf.Graph must be non-nil: NewScope→StepPathIndex→WalkNodes dereferences it.
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	oc, err := runSignalStep(context.Background(), ss, "wait_oob", wf, rs, nil, log, blobs, &clock.Fake{}, nil, b)
	if err != nil {
		t.Fatalf("runSignalStep: %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	nr, ok := rs.LookupCompleted("wait_oob")
	if !ok {
		t.Fatal("Completed[wait_oob] not set")
	}
	if nr.Outputs["candidate_id"] != "b" {
		t.Errorf("Outputs[candidate_id] = %v, want b (the where-matched payload)", nr.Outputs["candidate_id"])
	}
	// The non-matching seq 1 must still be buffered (consumable plainly).
	d1, err := b.Receive(context.Background(), "oob", 0)
	if err != nil {
		t.Fatalf("Receive remaining: %v", err)
	}
	if d1.Seq != 1 {
		t.Errorf("remaining signal seq=%d, want 1 (non-match must stay buffered)", d1.Seq)
	}
}

func TestRunSignalStep_WhereOuterRefFromEngineScope(t *testing.T) {
	// signal.candidate_id (payload scope) compared against run.id (outer engine
	// scope, constant across candidates — run.id == "r"). Both sides are
	// resolved to TYPED values and compared directly (F18: no string splice).
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"nope"}`)); err != nil {
		t.Fatalf("WriteSignal nope: %v", err)
	}
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"r"}`)); err != nil {
		t.Fatalf("WriteSignal r: %v", err)
	}
	ss := &ir.SignalStep{
		ID:           "wait_oob",
		Await:        "oob",
		Where:        `{{ signal.candidate_id == run.id }}`,
		OutputSchema: whereSchema(),
	}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	oc, err := runSignalStep(context.Background(), ss, "wait_oob", wf, rs, nil, log, blobs, &clock.Fake{}, nil, b)
	if err != nil {
		t.Fatalf("runSignalStep: %v", err)
	}
	if oc != OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	nr, _ := rs.LookupCompleted("wait_oob")
	if nr.Outputs["candidate_id"] != "r" {
		t.Errorf("Outputs[candidate_id] = %v, want r (slot-substituted match)", nr.Outputs["candidate_id"])
	}
}

func TestRunSignalStep_WhereNoMatchTimesOut(t *testing.T) {
	// One buffered signal that never matches → blocks → timeout → retryable.
	b := tempBroker(t)
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	if _, err := b.WriteSignal("oob", []byte(`{"candidate_id":"a"}`)); err != nil {
		t.Fatalf("WriteSignal a: %v", err)
	}
	timeout := ir.Duration(5 * time.Millisecond)
	ss := &ir.SignalStep{ID: "wait_oob", Await: "oob", Where: `{{ signal.candidate_id == "zzz" }}`, Timeout: &timeout}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	oc, err := runSignalStep(context.Background(), ss, "wait_oob", wf, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
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

func TestRunSignalStep_WhereMalformedGrammarIsPermanent(t *testing.T) {
	// F18: the ONLY synchronous where: failure mode left is a GRAMMAR parse
	// error (an author bug AWF1036 should have caught upstream) — never a ref
	// RESOLUTION failure (see WhereBadOuterRefTimesOut below). A truncated
	// expression inside a well-formed envelope fails template.ParseExpr before
	// the broker is ever touched → permanent_failure, not retryable.
	b := tempBroker(t) // no signals written — must fail before ever calling Receive
	rs := NewRunState("r", "d", nil)
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	ss := &ir.SignalStep{ID: "wait_oob", Await: "oob", Where: `{{ signal.candidate_id == }}`}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	oc, err := runSignalStep(context.Background(), ss, "wait_oob", wf, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err == nil {
		t.Fatal("err = nil, want permanent_failure")
	}
	if oc != OutcomePermanentFailure {
		t.Errorf("outcome = %q, want permanent_failure", oc)
	}
	if !strings.Contains(err.Error(), "where") {
		t.Errorf("err = %v, want 'where'", err)
	}
}

func TestRunSignalStep_WhereBadOuterRefIsPermanent(t *testing.T) {
	// F18 REGRESSION GUARD. An unresolvable OUTER ref in where:
	// (input.NONEXISTENT — not in the run input) must fail PERMANENTLY and FAST,
	// synchronously, BEFORE the broker is ever polled — NOT hang forever waiting
	// for a signal that can never match. The eager outer-ref pre-check in
	// buildWherePredicateWithScope resolves every non-signal ref once up front
	// and surfaces the typo as a permanent build error.
	//
	// Deliberately NO ss.Timeout is set: the whole point is that the failure is
	// synchronous, so no timeout is needed to bound it. A short CONTEXT deadline
	// is used purely as a test safety net — if this ever regresses back to the
	// deferred-in-MatchFunc behavior, the context cancels and the assertions
	// fail cleanly (retryable, not permanent) instead of wedging the suite.
	// Only signal.* refs stay deferred; outer roots are pre-checked.
	b := tempBroker(t) // no signals written — a correct impl never reaches Receive
	rs := NewRunState("r", "d", map[string]any{"present": "x"})
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	ss := &ir.SignalStep{ID: "wait_oob", Await: "oob", Where: `{{ signal.candidate_id == input.NONEXISTENT }}`}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	oc, err := runSignalStep(ctx, ss, "wait_oob", wf, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err == nil {
		t.Fatal("err = nil, want permanent_failure")
	}
	if oc != OutcomePermanentFailure {
		t.Errorf("outcome = %q, want permanent_failure (unresolvable outer ref must fail fast, not hang/timeout)", oc)
	}
	if !strings.Contains(err.Error(), "where") {
		t.Errorf("err = %v, want 'where'", err)
	}
	// Prove it failed BEFORE the broker was polled: the 2s safety-net deadline
	// must not have been consumed (a hang would have burned it).
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("context deadline was exceeded — where: outer-ref typo hung on the broker instead of failing fast")
	}
}

func TestRunSignalStep_WhereInjectionSafe(t *testing.T) {
	// LOAD-BEARING (F18 security proof). Historical vulnerability class
	// (pre-F18): the old substitute-then-parse builder rendered an outer ref's
	// value into the where: STRING via template.Substitute, then re-parsed
	// that string as an expression — a value containing predicate
	// metacharacters (quotes/parens/boolean operators) could alter the parsed
	// expression's STRUCTURE (classic injection). F18 deletes that splice:
	// every ref — signal.* AND outer — resolves to a TYPED value compared by
	// the evaluator's compare(), which never re-enters the parser.
	//
	// Prove it: drive the SAME malicious string through BOTH channels at once
	// — an outer ref (input.expected) and a signal payload field
	// (signal.note) — as the comparison operand for a candidate whose
	// candidate_id does NOT match. If injection still worked, splicing the
	// malicious value would corrupt the predicate into something that matches
	// every candidate; if it's inert data, this candidate correctly never
	// matches and the await times out with the signal still buffered.
	const malicious = `") || true) -- `

	b := tempBroker(t)
	rs := NewRunState("r", "d", map[string]any{"expected": malicious})
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	payloadBytes, err := json.Marshal(map[string]any{"candidate_id": "a", "note": malicious})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := b.WriteSignal("oob", payloadBytes); err != nil {
		t.Fatalf("WriteSignal: %v", err)
	}
	timeout := ir.Duration(5 * time.Millisecond)
	ss := &ir.SignalStep{
		ID:    "wait_oob",
		Await: "oob",
		// signal.note == input.expected is TRUE (both are the same malicious
		// string, compared as data) but signal.candidate_id == input.expected
		// is FALSE — candidate_id ("a") was never near the malicious value, so
		// injection would have to corrupt this clause's grammar to force a
		// match. It doesn't: the whole expression evaluates false.
		Where:   `{{ signal.candidate_id == input.expected && signal.note == input.expected }}`,
		Timeout: &timeout,
	}
	wf := &ir.Workflow{ID: "w", Version: 1, Graph: ir.NodeList{ss}}
	oc, err := runSignalStep(context.Background(), ss, "wait_oob", wf, rs, nil, log, state.NewInMemoryBlobs(), &clock.Fake{}, nil, b)
	if err == nil {
		t.Fatal("err = nil, want timeout (no injection: candidate_id \"a\" != malicious value)")
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome = %q, want retryable_failure (malicious value must NOT force a match)", oc)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want 'timeout'", err)
	}
	// The buffered signal must remain UNCONSUMED — proves the predicate was
	// never corrupted into an always-true match.
	d, rerr := b.Receive(context.Background(), "oob", 0)
	if rerr != nil {
		t.Fatalf("Receive remaining: %v", rerr)
	}
	if d.Seq != 1 {
		t.Errorf("remaining signal seq=%d, want 1 (candidate must still be buffered, unconsumed)", d.Seq)
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
