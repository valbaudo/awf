package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testPinning is Bucket 1 — digest-mismatch hard error on resume (spec §8).
func testPinning(t *testing.T, factory BackendFactory) {
	t.Helper()

	t.Run("digest-mismatch-hard-error", func(t *testing.T) {
		wrappedFactory := preProgramFake(t, factory, []execProgram{
			{cmd: "./step1.sh", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./step2.sh", res: container.ExecResult{ExitCode: 0}},
		})
		h := newHarness(t, wrappedFactory, tinySeqWorkflow)

		outcome, err := h.runWorkflow(t)
		if err != nil {
			t.Fatalf("first run: %v", err)
		}
		if outcome != engine.OutcomeOK {
			t.Fatalf("first run outcome = %v, want ok", outcome)
		}
		preMutationEventCount := len(mustFoldEvents(t, h))

		if err := os.WriteFile(h.wfPath, []byte(tinySeqWorkflowMutated), 0o644); err != nil {
			t.Fatalf("WriteFile mutated: %v", err)
		}

		_, err = h.resumeWorkflow(t)
		if err == nil {
			t.Fatal("resume against mutated workflow: err = nil, want digest-mismatch error")
		}
		var dme *digestMismatchError
		if !errors.As(err, &dme) {
			t.Errorf("err = %v, want *digestMismatchError", err)
		}

		postEvents := mustFoldEvents(t, h)
		if len(postEvents) != preMutationEventCount {
			t.Errorf("log advanced past refusal: pre=%d, post=%d events", preMutationEventCount, len(postEvents))
		}
		for _, e := range postEvents {
			if e.Type == engine.EventRunResumed {
				t.Errorf("digest-mismatch refusal must NOT append run.resumed; events: %+v", postEvents)
			}
		}
	})

	t.Run("runtime-version-drift", func(t *testing.T) {
		t.Skip("Phase 2 has no `uses:` execution; Phase 5 lights this up")
	})

	t.Run("asset-digest-mismatch-hard-error", func(t *testing.T) {
		h := newHarness(t, factory, `workflow: asset-pinning
version: 1
assets:
  input: assets/input.txt
containers: {}
graph: []
`)
		assetPath := filepath.Join(h.baseDir, "assets", "input.txt")
		if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
			t.Fatalf("MkdirAll assets: %v", err)
		}
		if err := os.WriteFile(assetPath, []byte("original"), 0o644); err != nil {
			t.Fatalf("WriteFile asset: %v", err)
		}

		outcome, err := h.runWorkflow(t)
		if err != nil {
			t.Fatalf("first run: %v", err)
		}
		if outcome != engine.OutcomeOK {
			t.Fatalf("first run outcome = %v, want ok", outcome)
		}
		preMutationEvents := mustFoldEvents(t, h)
		var started engine.RunStartedData
		if err := json.Unmarshal(preMutationEvents[0].Data, &started); err != nil {
			t.Fatalf("unmarshal run.started: %v", err)
		}
		if len(started.Assets) != 1 || len(started.Assets["input"].Files) != 1 {
			t.Fatalf("run.started assets = %#v, want input manifest", started.Assets)
		}

		if err := os.WriteFile(assetPath, []byte("mutated"), 0o644); err != nil {
			t.Fatalf("mutate asset: %v", err)
		}

		_, err = h.resumeWorkflow(t)
		if err == nil {
			t.Fatal("resume against mutated asset: err = nil, want digest-mismatch error")
		}
		var dme *digestMismatchError
		if !errors.As(err, &dme) {
			t.Errorf("err = %v, want *digestMismatchError", err)
		}
		postEvents := mustFoldEvents(t, h)
		if len(postEvents) != len(preMutationEvents) {
			t.Errorf("log advanced past asset drift refusal: pre=%d, post=%d events", len(preMutationEvents), len(postEvents))
		}
		for _, e := range postEvents {
			if e.Type == engine.EventRunResumed {
				t.Errorf("asset digest-mismatch refusal must NOT append run.resumed; events: %+v", postEvents)
			}
		}
	})
}
