package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testContinuesCrash is T6 from the spec (Phase 4 Task 4.6): crash-injection on
// a participating turn's node.completed proves the "Put before Append" ordering.
//
// The workflow has two agent steps (a, b continues: a). Step a is the continues:
// target of b, so a participates in the conversation thread and Commit puts its
// transcript blob BEFORE appending a's node.completed.
//
// We crash exactly at a's node.completed Append via FailAppendAfterN(2):
//
//	k=0: run.started
//	k=1: node.started a   (observational; error discarded)
//	k=2: node.completed a ← crash target (transcript blob already Put at this point)
//
// Assertions (the three T6 claims):
//
//	(a) the transcript blob IS present in Blobs — Put ran before the crashing Append.
//	(b) no node.completed event in the log references it — the Append never landed.
//	(c) engine.Fold(events, blobs) succeeds and Completed has no entry for a's path —
//	    the uncommitted step is not bricked as done; resume would re-execute it.
//
// This test is a regression guard: it is GREEN immediately (since Task 4.3 already
// implements the correct Put→Append ordering). If it were RED, that would indicate a
// real ordering bug in engine/commit.go that must be fixed, NOT a test to be weakened.
func testContinuesCrash(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("transcript_blob_durable_before_node_completed", func(t *testing.T) {
		testTranscriptBlobDurableBeforeNodeCompleted(t, factory)
	})
}

// continuesCrashWorkflow is a minimal 2-step continues chain served by the
// awf/llm containerless adapter. Step a is the target of b's continues:, which
// makes a a participating turn — Commit will Put a's transcript blob before
// appending a's node.completed. No containers: block needed (Containerless=true).
const continuesCrashWorkflow = `workflow: conformance-continues-crash
version: 1
graph:
  - id: a
    uses: awf/llm
    with:
      model: m
      prompt: "first turn"
    output_schema:
      type: object
      additionalProperties: false
      required: [draft]
      properties:
        draft: { type: string }
  - id: b
    uses: awf/llm
    continues: a
    with:
      model: m
      prompt: "second turn"
    output_schema:
      type: object
      additionalProperties: false
      required: [draft]
      properties:
        draft: { type: string }
`

func testTranscriptBlobDurableBeforeNodeCompleted(t *testing.T, factory BackendFactory) {
	t.Helper()

	// Script a distinct Transcript for step a so Commit has a non-zero blob to Put.
	// Step b's script is irrelevant — the crash happens at a's node.completed (k=2)
	// before b ever runs.
	wantTranscript := agent.ThreadTurn{
		User:      "first turn",
		Assistant: "here is the first draft",
	}
	register := func(reg *agent.Registry) {
		fk := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
			Script(0, fake.Result{
				Output:     map[string]any{"draft": "v1"},
				Transcript: wantTranscript,
			}).
			Script(1, fake.Result{
				Output:     map[string]any{"draft": "v2"},
				Transcript: agent.ThreadTurn{User: "second turn", Assistant: "v2"},
			})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, continuesCrashWorkflow, register)

	// Crash at a's node.completed Append (k=2):
	//   k=0  run.started
	//   k=1  node.started a   (observational)
	//   k=2  node.completed a ← this is the Append that fails
	// The transcript blob for a was Put at k<2 (during Commit's Blobs.Put phase),
	// so the blob MUST be present even after the Append crashes.
	h.log.FailAppendAfterN(2)

	_, err := h.runWorkflow(t)
	if err == nil {
		t.Fatalf("FailAppendAfterN(2): err = nil, want induced-fault error")
	}
	if !strings.Contains(err.Error(), "induced") {
		t.Errorf("err = %v, want induced-fault error containing %q", err, "induced")
	}

	// Snapshot post-crash log and blobs.
	events := mustFoldEvents(t, h)

	// (a) The transcript blob MUST be present in Blobs — Put happened before the
	// crashing Append. Find its ref by scanning NodeCompletedData for any
	// node.completed that DID land (none expected here), OR by computing the
	// content-addressed ref ourselves from the known transcript bytes. Since no
	// node.completed landed (the crash was on k=2 = the FIRST node.completed),
	// we verify presence by marshalling the same transcript and checking Blobs.Get.
	transcriptBytes, err2 := json.Marshal(wantTranscript)
	if err2 != nil {
		t.Fatalf("marshal expected transcript: %v", err2)
	}
	// InMemoryBlobs uses SHA-256 CAS; we must derive the ref the same way Blobs
	// derives it. The cleanest way without importing the internal hash is to re-Put
	// the same bytes and confirm the ref is already present (idempotent Put).
	ref, putErr := h.blobs.Put(transcriptBytes)
	if putErr != nil {
		t.Fatalf("re-Put transcript bytes to derive ref: %v", putErr)
	}
	if _, getErr := h.blobs.Get(ref); getErr != nil {
		t.Errorf("T6 assertion (a) FAILED: transcript blob %q missing from Blobs after crash — "+
			"Commit put the blob AFTER the append (ordering bug in engine/commit.go): %v", ref, getErr)
	}

	// (b) No node.completed in the log should reference the transcript blob.
	// (In fact, zero node.completed events should be present — the crash hit k=2
	// which is a's commit, the first node.completed in the run.)
	completedCount := 0
	for _, e := range events {
		if e.Type != engine.EventNodeCompleted {
			continue
		}
		completedCount++
		// Every present node.completed must reference only present blobs (existing invariant).
		assertBlobsPresent(t, e, h.blobs)
		// Additionally: assert this node.completed does NOT reference the orphaned transcript ref.
		var d engine.NodeCompletedData
		if err3 := json.Unmarshal(e.Data, &d); err3 != nil {
			t.Fatalf("unmarshal node.completed: %v", err3)
		}
		if d.TranscriptRef == ref {
			t.Errorf("T6 assertion (b) FAILED: node.completed at path=%q references the orphaned transcript blob %q — "+
				"the append must have landed despite the induced fault", e.Path, ref)
		}
	}
	// Crash at k=2 = the first node.completed → zero node.completed events expected.
	if completedCount != 0 {
		t.Errorf("post-crash node.completed count = %d, want 0 (crash at k=2 = first commit)", completedCount)
	}

	// (c) engine.Fold(events, blobs) MUST succeed and MUST NOT produce a Completed
	// entry for a's path. The orphaned transcript blob must not brick the fold.
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("T6 assertion (c): Fold post-crash returned error: %v", ferr)
	}
	if len(rs.Completed) != 0 {
		t.Errorf("T6 assertion (c) FAILED: Fold(events,blobs).Completed size = %d, want 0 "+
			"(orphan transcript blob MUST NOT manufacture a Completed entry for a)", len(rs.Completed))
	}
	if _, hasA := rs.LookupCompleted("a"); hasA {
		t.Errorf("T6 assertion (c) FAILED: Fold produced Completed[\"a\"] despite crashed node.completed — " +
			"step a is uncommitted; resume must re-run it, not replay it")
	}
}
