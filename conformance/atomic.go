package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// testAtomic is Bucket 3 — atomic commit (spec section 8 + design spec
// section H). The commit boundary is "content-address-then-pointer-swap"
// (CLAUDE.md invariant): Blobs.Put first, then Log.Append(node.completed)
// with the refs. A crash BETWEEN the two leaves orphan blobs (acceptable
// per section 11.5 GC-deferred) but MUST NOT produce a node.completed
// event referencing a missing blob.
//
// The bucket asserts the invariant from the OTHER direction too: the
// log's fold MUST NOT manufacture a Completed[path] entry for orphan
// blobs. Only node.completed events drive RunState.Completed — Phase
// 2's fold (engine.Fold, slice 2.1) is the load-bearing test.
//
// Mechanics: FailAppendAfterN(k) crashes the (k+1)-th Append. For a
// 2-step sequential workflow with no retries, the Append sequence is:
//
//	k=0: run.started   (we don't crash this — pre-run failure isn't
//	     the commit-atomicity concern)
//	k=1: node.completed for step 1 (THE crash we want for step 1)
//	k=2: node.completed for step 2
//
// So k=1 crashes step 1's commit AFTER its Blobs.Put; orphan blobs
// exist but no node.completed.
func testAtomic(t *testing.T, factory BackendFactory) {
	t.Helper()

	for _, crashStep := range []int{1, 2} {
		crashStep := crashStep
		t.Run("commit-crash-step-"+stepName(crashStep), func(t *testing.T) {
			// Pre-program the fake for both steps with outputs +
			// stdout so Blobs.Put runs (and orphans the blob).
			programs := []execProgram{
				{
					cmd: "./step1.sh",
					res: container.ExecResult{
						ExitCode:  0,
						Stdout:    []byte("hello from step1\n"),
						AWFOutput: []byte(`{"k1":"v1"}`),
					},
				},
				{
					cmd: "./step2.sh",
					res: container.ExecResult{
						ExitCode:  0,
						Stdout:    []byte("hello from step2\n"),
						AWFOutput: []byte(`{"k2":"v2"}`),
					},
				},
			}
			wrappedFactory := preProgramFake(t, factory, programs)
			h := newHarness(t, wrappedFactory, tinySeqWorkflow)

			// Program FailAppendAfterN(crashStep) on the harness's log.
			// crashStep=1 → 2nd Append crashes (node.completed_1, after
			//  run.started). Blobs.Put for step 1 already ran → orphan
			//  blobs exist.
			// crashStep=2 → 3rd Append crashes (node.completed_2).
			h.log.FailAppendAfterN(crashStep)

			// Run. engine.Run returns non-ok + error — the
			// interpreter's Commit failure is an internal-error class
			// (slice 2.5 Design question 4: empty Outcome + non-nil
			// err). The harness returns whatever engine.Run produced.
			_, err := h.runWorkflow(t)
			if err == nil {
				t.Fatalf("FailAppendAfterN(%d): err = nil, want induced-fault error", crashStep)
			}
			// The InMemoryLog fault wraps its message with "induced
			// Append fault" (state/fake.go:36). String-match keeps this
			// independent of whether state/fake.go later promotes the
			// sentinel to an exported var.
			if !strings.Contains(err.Error(), "induced") {
				t.Errorf("err = %v, want induced-fault error", err)
			}

			// Snapshot post-crash log + blobs. Every node.completed
			// event MUST reference a present blob.
			events := mustFoldEvents(t, h)
			completedCount := 0
			for _, e := range events {
				if e.Type != engine.EventNodeCompleted {
					continue
				}
				completedCount++
				assertBlobsPresent(t, e, h.blobs)
			}
			// crashStep=1 → 0 node.completed events (step 1 never landed).
			// crashStep=2 → 1 node.completed event (step 1 committed,
			//   step 2's commit crashed).
			wantCompleted := crashStep - 1
			if completedCount != wantCompleted {
				t.Errorf("post-crash node.completed count = %d, want %d", completedCount, wantCompleted)
			}

			// Fold MUST NOT manufacture a Completed entry for orphan
			// blobs. engine.Fold(events, blobs) returns a RunState
			// whose Completed map size equals the node.completed count.
			rs, ferr := engine.Fold(events, h.blobs)
			if ferr != nil {
				t.Fatalf("Fold post-crash: %v", ferr)
			}
			if len(rs.Completed) != wantCompleted {
				t.Errorf("Fold(events,blobs).Completed size = %d, want %d (orphan blobs MUST NOT manufacture completions)",
					len(rs.Completed), wantCompleted)
			}
		})
	}
}

func stepName(i int) string {
	if i == 1 {
		return "step1"
	}
	return "step2"
}

// assertBlobsPresent fails the test if any blob ref in a node.completed
// payload is missing from blobs. Closes the loop on the spec section 8
// invariant: "a 'done' record must never reference a missing artifact."
func assertBlobsPresent(t *testing.T, e state.Event, blobs *state.InMemoryBlobs) {
	t.Helper()
	var d engine.NodeCompletedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("unmarshal node.completed at seq=%d: %v", e.Seq, err)
	}
	refs := []string{}
	if d.OutputsRef != "" {
		refs = append(refs, d.OutputsRef)
	}
	if d.StdoutRef != "" {
		refs = append(refs, d.StdoutRef)
	}
	for _, ref := range d.Files {
		refs = append(refs, ref)
	}
	for _, ref := range refs {
		if _, err := blobs.Get(ref); err != nil {
			t.Errorf("node.completed at path=%q references missing blob %q (spec section 8 atomic-commit violation): %v",
				e.Path, ref, err)
		}
	}
}
