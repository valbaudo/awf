package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// testAgentStep is Bucket 12 (Phase 5 slice 5.2). Three sub-tests:
//
//   - typed_output_committed: agent/fake.Launch returns typed Output;
//     node.completed has Outputs that match the schema; downstream refs
//     would see them (no downstream step in this fixture — the round-trip
//     through Blobs is the assertion).
//   - validate_rejects_unknown_with: a rejectingAdapter registered under
//     the same Ref makes the dispatch fail with *agent.ErrInvalidConfig →
//     permanent_failure; run halts.
//   - unresolved_uses_halts_run: AgentStep references a `uses:` ref no
//     registered adapter satisfies → *agent.ErrAdapterNotFound surfaces
//     as an internal halt (engine.Run returns ("", wrappedError) — no
//     node.failed entry; see engine/agent_step.go:122-126 for the
//     dr.Outcome == "" split). (Engine-side peer of slice 5.1's
//     cli/resume drift check — see plan prose for the re-interpretation.)
func testAgentStep(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("typed_output_committed", func(t *testing.T) { testAgentTypedOutputCommitted(t, factory) })
	t.Run("validate_rejects_unknown_with", func(t *testing.T) { testAgentValidateRejects(t, factory) })
	t.Run("unresolved_uses_halts_run", func(t *testing.T) { testAgentUnresolvedUsesHalts(t, factory) })
	t.Run("containerless_commits_and_resumes", func(t *testing.T) { testAgentContainerlessCommits(t, factory) })
	t.Run("containerless_input_files_delivery", func(t *testing.T) { testAgentContainerlessInputFiles(t, factory) })
}

func testAgentTypedOutputCommitted(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
			Output: map[string]any{"verdict": "approved", "confidence": 0.95},
		})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	// Read node.completed via RunState (built by Fold in the harness).
	events, ferr := h.log.Fold()
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("engine.Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("triage")
	if !ok {
		t.Fatalf("Completed[triage] missing")
	}
	if nr.Outputs["verdict"] != "approved" {
		t.Errorf("Outputs[verdict] = %v, want %q", nr.Outputs["verdict"], "approved")
	}
}

func testAgentValidateRejects(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		if err := reg.Register(&validateRejecter{
			Fake: fake.New("anthropic/claude-code"),
			err:  &agent.ErrInvalidConfig{Ref: "anthropic/claude-code", Key: "prompt", Reason: "forbidden"},
		}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomePermanentFailure)
	}
	var badConfig *agent.ErrInvalidConfig
	if !errors.As(err, &badConfig) {
		t.Errorf("err = %v, want *agent.ErrInvalidConfig", err)
	}
}

func testAgentUnresolvedUsesHalts(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Register an adapter under a DIFFERENT ref than the workflow names.
	// Resolver.Lookup("anthropic/claude-code") misses → dispatcher returns
	// *agent.ErrAdapterNotFound → dispatcher returns it as a plain error with
	// Outcome==""; runAgentStep's dr.Outcome=="" branch returns ("", err) — an
	// internal halt, NOT permanent_failure. Run halts with non-nil error.
	// This is the engine-surface peer of slice 5.1's cli/resume
	// drift check (which is fully covered by TestErrRuntimeDrift_* in
	// cli/resume_test.go); slice 5.2 does NOT duplicate that coverage.
	register := func(reg *agent.Registry) {
		if err := reg.Register(fake.New("test/some-other-adapter")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	// ErrAdapterNotFound is an infrastructure miss (no adapter registered for the
	// ref). The dispatcher returns it as a plain error with Outcome=="", so
	// runAgentStep propagates it as ("", error) — an internal halt, NOT a step
	// node.failed permanent_failure. The run halts with a non-nil error; the
	// outcome is the empty string.
	if err == nil {
		t.Fatalf("expected non-nil error for unresolved adapter, got oc=%q", oc)
	}
	var notFound *agent.ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want *agent.ErrAdapterNotFound", err)
	}
	if notFound.Ref != "anthropic/claude-code" {
		t.Errorf("Ref = %q, want %q", notFound.Ref, "anthropic/claude-code")
	}
}

func testAgentContainerlessCommits(t *testing.T, factory BackendFactory) {
	t.Helper()
	var fk *fake.Fake // captured so we can assert the Launch count after resume (N5)
	register := func(reg *agent.Registry) {
		fk = fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
			Script(0, fake.Result{Output: map[string]any{"answer": "42"}})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepContainerlessWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	events, ferr := h.log.Fold()
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("engine.Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("ask")
	if !ok {
		t.Fatalf("Completed[ask] missing")
	}
	if nr.Outputs["answer"] != "42" {
		t.Errorf("Outputs[answer] = %v, want %q", nr.Outputs["answer"], "42")
	}
	// Resume: committed step replays from the log (no infra rebuilt, no
	// container declared). Outcome stays ok, no second Launch.
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("resumeWorkflow: %v", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("resume Outcome = %q, want %q", oc2, engine.OutcomeOK)
	}
	// Replay, NOT recompute (CLAUDE.md invariant): the committed step is not
	// re-dispatched on resume, so the fake saw exactly ONE Launch across run+resume.
	// (Same fake instance persists — newHarnessWithAgentRegistry registers it once and
	// both runOrResume calls reuse h.agentRegistry.)
	if n := len(fk.Calls()); n != 1 {
		t.Errorf("fake Launch count across run+resume = %d, want 1 (replayed, not recomputed)", n)
	}
}

// reconMarker is a recognizable byte sequence embedded in the test PDF. The
// no-bytes-in-log assertion scans every journaled event for this marker (and its
// base64): if either appears, input bytes leaked into the durable state log.
const reconMarker = "%PDF-RECONMARKER"

// testAgentContainerlessInputFiles is the Task 12 conformance proof for the
// containerless input_files channel. It drives a CONTAINERLESS agent step that
// receives a PDF via input_files end-to-end through engine.Run against the FAKE
// backend + a recording fake adapter, and proves three things:
//
//  1. The step runs to completion: a node.completed is journaled for "ask".
//  2. The adapter actually RECEIVED the file: the recorded AgentInvocation's
//     InputFiles has one entry with Name "doc" and MIME "application/pdf".
//  3. No-bytes-in-log (H3 invariant): the raw PDF marker and its base64 appear in
//     NO journaled event — confirming AgentInvocation.InputFiles' json:"-" tag and
//     the emit path keep input bytes out of the durable log.
//
// DEVIATION — input.files.<name> vs asset.<id>: the feature resolves both a
// workflow input file (input.files.<name>) and a single-file asset (asset.<id>)
// through the SAME containerless resolver (engine.resolveContainerlessInputFiles
// -> resolveSingleRefBytes -> DetectMIME -> agent.InputFile), and both converge
// on the identical dispatcher threading (ResolvedInputs.ContainerlessFiles ->
// AgentInvocation.InputFiles, json:"-"). But only asset.<id> is wired end-to-end
// through engine.Run + the fake-backend harness: the top-level input.files.<name>
// channel is reachable only by constructing a Scope directly (covered by
// engine/input_files_containerless_test.go) — engine.Run never populates the root
// ictx.inputFiles, and no RunOptions/CLI mechanism supplies it yet. So this
// conformance test exercises the run-start file channel the fake-backend harness
// genuinely supports (asset.<id>); the delivery + no-bytes-in-log behavior under
// test is identical for both ref forms.
func testAgentContainerlessInputFiles(t *testing.T, factory BackendFactory) {
	t.Helper()
	// The asset bytes: a valid PDF (starts with "%PDF-" so agent.DetectMIME sniffs
	// application/pdf) carrying the recognizable reconMarker for the no-bytes scan.
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n" + reconMarker + "\n1 0 obj\n<< >>\nendobj\n")

	var fk *fake.Fake // captured so we can inspect the recorded invocation after the run
	register := func(reg *agent.Registry) {
		fk = fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
			Script(0, fake.Result{Output: map[string]any{"answer": "summarized"}})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepContainerlessInputFilesWorkflow, register)
	// The asset path "doc.pdf" is relative to the workflow file; write it next to
	// workflow.yaml in the harness's shared baseDir so loader.Load resolves it.
	if err := os.WriteFile(filepath.Join(h.baseDir, "doc.pdf"), pdf, 0o644); err != nil {
		t.Fatalf("write doc.pdf: %v", err)
	}

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	// (1) node.completed journaled for the containerless step.
	events := mustFoldEvents(t, h)
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("engine.Fold: %v", ferr)
	}
	if _, ok := rs.LookupCompleted("ask"); !ok {
		t.Fatalf("Completed[ask] missing — containerless input_files step did not commit")
	}

	// (2) the adapter RECEIVED the file as an inline part with the right Name+MIME.
	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake Launch count = %d, want 1", len(calls))
	}
	got := calls[0].InputFiles
	if len(got) != 1 {
		t.Fatalf("AgentInvocation.InputFiles len = %d, want 1", len(got))
	}
	if got[0].Name != "doc" {
		t.Errorf("InputFiles[0].Name = %q, want %q", got[0].Name, "doc")
	}
	if got[0].MIME != "application/pdf" {
		t.Errorf("InputFiles[0].MIME = %q, want %q", got[0].MIME, "application/pdf")
	}
	if !bytes.Equal(got[0].Content, pdf) {
		t.Errorf("InputFiles[0].Content did not round-trip the asset bytes")
	}

	// (3) No-bytes-in-log (H3): the PDF marker AND its base64 must appear in NO
	// journaled event. Marshal every folded event (Seq/Epoch/TS/Path/Type/
	// PayloadRef/Data) and scan the raw JSON — catches any leak into Data or a
	// PayloadRef-addressed inline payload. The marker is distinctive enough that a
	// hit means the input bytes reached the durable state log.
	assertNoInputBytesInLog(t, events, pdf)
}

// assertNoInputBytesInLog marshals every journaled event and fails if the raw
// content marker OR its base64 encoding appears anywhere in the log. This is the
// H3 invariant check: AgentInvocation.InputFiles is json:"-", so input bytes must
// never reach a node.completed (or any other) event.
func assertNoInputBytesInLog(t *testing.T, events []state.Event, content []byte) {
	t.Helper()
	rawMarker := []byte(reconMarker)
	b64Marker := []byte(base64.StdEncoding.EncodeToString(content))
	for _, ev := range events {
		blob, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event seq=%d: %v", ev.Seq, err)
		}
		if bytes.Contains(blob, rawMarker) {
			t.Fatalf("raw input bytes leaked into the state log (event seq=%d type=%q): %s", ev.Seq, ev.Type, blob)
		}
		if bytes.Contains(blob, b64Marker) {
			t.Fatalf("base64 of input bytes leaked into the state log (event seq=%d type=%q): %s", ev.Seq, ev.Type, blob)
		}
	}
}

// validateRejecter is a fake.Fake subclass that overrides ValidateConfig
// for Bucket 12 validate-rejects sub-test. (engine/local_dispatcher_test.go
// has a sibling type with the same purpose; can't share — _test.go files
// don't cross packages.)
type validateRejecter struct {
	*fake.Fake
	err error
}

func (r *validateRejecter) ValidateConfig(_ ir.RawConfig) error { return r.err }
