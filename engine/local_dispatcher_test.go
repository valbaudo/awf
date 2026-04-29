package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func newDispatcher(t *testing.T) (*engine.LocalDispatcher, *container.Fake, container.Handle) {
	t.Helper()
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": h},
	}
	return d, fake, h
}

func TestLocalDispatcherHappyPath(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./triage.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"web_exploitable":true,"detections":3}`),
		Stdout:    []byte("running triage...\n"),
	}, nil)

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"web_exploitable", "detections"},
		"properties": map[string]any{
			"web_exploitable": map[string]any{"type": "boolean"},
			"detections":      map[string]any{"type": "integer"},
		},
	}
	intent := engine.NodeIntent{
		Path: "triage",
		Node: &ir.CodeStep{ID: "triage", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./triage.sh",
			Env:          map[string]string{},
			OutputSchema: &schema,
		},
	}
	dr, ch, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	if dr.ExitCode == nil || *dr.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want *int(0)", dr.ExitCode)
	}
	if dr.Outputs["web_exploitable"] != true {
		t.Errorf("Outputs.web_exploitable = %v, want true", dr.Outputs["web_exploitable"])
	}
	if dr.Outputs["detections"] != float64(3) {
		t.Errorf("Outputs.detections = %v, want 3", dr.Outputs["detections"])
	}
	if string(dr.Stdout) != "running triage...\n" {
		t.Errorf("Stdout = %q, want %q", dr.Stdout, "running triage...\n")
	}
	if ch == nil {
		t.Error("IOChunk channel is nil; want closed-but-non-nil")
	}
	for range ch {
	}
}

func TestLocalDispatcherIdempotencyKeyReachesBackend(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./open-pr.sh", container.ExecResult{ExitCode: 0}, nil)
	intent := engine.NodeIntent{
		Path: "open_pr",
		Node: &ir.CodeStep{ID: "open_pr", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./open-pr.sh",
			Env:     map[string]string{"X": "y"},
		},
		IdempotencyKey: "CVE-2024-9999:pr",
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fake.Calls))
	}
	gotEnv := fake.Calls[0].Env
	if gotEnv["AWF_IDEMPOTENCY_KEY"] != "CVE-2024-9999:pr" {
		t.Errorf("Backend.Exec received AWF_IDEMPOTENCY_KEY = %q, want %q", gotEnv["AWF_IDEMPOTENCY_KEY"], "CVE-2024-9999:pr")
	}
	if gotEnv["X"] != "y" {
		t.Errorf("Backend.Exec received X = %q, want %q (pre-existing env must carry through)", gotEnv["X"], "y")
	}
	if _, present := intent.ResolvedInputs.Env["AWF_IDEMPOTENCY_KEY"]; present {
		t.Errorf("dispatcher mutated caller's Env map (AWF_IDEMPOTENCY_KEY leaked back)")
	}
}

func TestLocalDispatcherNoIdempotencyKeyMeansNoEnvVar(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./noop.sh", container.ExecResult{ExitCode: 0}, nil)
	intent := engine.NodeIntent{
		Path: "noop",
		Node: &ir.CodeStep{ID: "noop", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./noop.sh",
		},
	}
	_, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, present := fake.Calls[0].Env["AWF_IDEMPOTENCY_KEY"]; present {
		t.Errorf("AWF_IDEMPOTENCY_KEY was injected with empty IdempotencyKey; want absent")
	}
}

func TestLocalDispatcherNonzeroExitIsRetryable(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./flaky.sh", container.ExecResult{
		ExitCode: 1, Stdout: []byte("transient error\n"),
	}, nil)
	intent := engine.NodeIntent{
		Path: "flaky",
		Node: &ir.CodeStep{ID: "flaky", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:               "./flaky.sh",
			NonRetryableExitCodes: []int{78},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
}

func TestLocalDispatcherPermanentExitCode(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./misconfig.sh", container.ExecResult{ExitCode: 78}, nil)
	intent := engine.NodeIntent{
		Path: "misconfig",
		Node: &ir.CodeStep{ID: "misconfig", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:               "./misconfig.sh",
			NonRetryableExitCodes: []int{78},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent", dr.Outcome)
	}
}

func TestLocalDispatcherUnparseableAWFOutputIsRetryable(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./broken.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`not valid json`),
	}, nil)
	schema := ir.JSONSchema{"type": "object", "additionalProperties": false}
	intent := engine.NodeIntent{
		Path: "broken",
		Node: &ir.CodeStep{ID: "broken", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./broken.sh",
			OutputSchema: &schema,
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
	if dr.Err == nil {
		t.Error("DispatchResult.Err is nil; want parse error")
	}
}

func TestLocalDispatcherSchemaInvalidAWFOutputIsRetryable(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./schemafail.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"web_exploitable":"not-a-bool"}`),
	}, nil)
	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"web_exploitable"},
		"properties": map[string]any{
			"web_exploitable": map[string]any{"type": "boolean"},
		},
	}
	intent := engine.NodeIntent{
		Path: "schemafail",
		Node: &ir.CodeStep{ID: "schemafail", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./schemafail.sh",
			OutputSchema: &schema,
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
}

func TestLocalDispatcherTransportErrorIsRetryable(t *testing.T) {
	d, _, _ := newDispatcher(t)
	intent := engine.NodeIntent{
		Path: "missing",
		Node: &ir.CodeStep{ID: "missing", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./never-programmed.sh",
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
	if dr.Err == nil {
		t.Error("DispatchResult.Err is nil; want transport error")
	}
}

func TestLocalDispatcherCapturesOutputFiles(t *testing.T) {
	d, fake, h := newDispatcher(t)
	fake.ProgramExec("./make-files.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(h, "/out/report.json", []byte(`{"k":"v"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	intent := engine.NodeIntent{
		Path: "make_files",
		Node: &ir.CodeStep{ID: "make_files", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:     "./make-files.sh",
			OutputFiles: []string{"/out/report.json"},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	if len(dr.Files) != 1 || dr.Files[0].Path != "/out/report.json" || string(dr.Files[0].Content) != `{"k":"v"}` {
		t.Errorf("Files = %+v, want one file with content {\"k\":\"v\"}", dr.Files)
	}
}

func TestLocalDispatcherMissingOutputFileIsRetryable(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./forgot.sh", container.ExecResult{ExitCode: 0}, nil)
	intent := engine.NodeIntent{
		Path: "forgot",
		Node: &ir.CodeStep{ID: "forgot", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:     "./forgot.sh",
			OutputFiles: []string{"/out/never-created"},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
}

func TestLocalDispatcherUnknownContainerIsError(t *testing.T) {
	d, _, _ := newDispatcher(t)
	intent := engine.NodeIntent{
		Path: "unknown",
		Node: &ir.CodeStep{ID: "unknown", Container: "no_such_container"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./whatever.sh",
		},
	}
	_, _, err := d.Run(context.Background(), intent)
	if err == nil {
		t.Fatal("expected error for unknown container, got nil")
	}
	if errors.Is(err, engine.ErrUnsupportedKind) {
		t.Errorf("err = %v wraps ErrUnsupportedKind; want a distinct 'unknown container' error", err)
	}
}

func TestLocalDispatcherAgentStepReturnsUnsupportedKind(t *testing.T) {
	d, _, _ := newDispatcher(t)
	intent := engine.NodeIntent{
		Path: "ag",
		Node: &ir.AgentStep{ID: "ag", Container: "lab", Uses: "anthropic/claude-code"},
	}
	_, _, err := d.Run(context.Background(), intent)
	if !errors.Is(err, engine.ErrUnsupportedKind) {
		t.Errorf("err = %v, want ErrUnsupportedKind", err)
	}
}

func TestLocalDispatcherSignalStepReturnsUnsupportedKind(t *testing.T) {
	d, _, _ := newDispatcher(t)
	intent := engine.NodeIntent{
		Path: "sig",
		Node: &ir.SignalStep{ID: "sig", Await: "human_review"},
	}
	_, _, err := d.Run(context.Background(), intent)
	if !errors.Is(err, engine.ErrUnsupportedKind) {
		t.Errorf("err = %v, want ErrUnsupportedKind", err)
	}
}

func TestLocalDispatcherCancelledContextIsRetryable(t *testing.T) {
	// A pre-cancelled ctx makes Backend.Exec return ctx.Err() (Task 6's fake
	// fix). The dispatcher classifies that as a transport-class callErr →
	// retryable_failure. Confirms ctx-cancellation routing.
	//
	// Note: this test does NOT exercise the dispatcher's context.WithTimeout
	// block (Timeout-field path). A blocking fake that actually times out is
	// Phase 4 Docker testing territory — the Phase 2 fake returns synchronously
	// from its scripted table, so an applied WithTimeout has no observable
	// behavior here.
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./irrelevant.sh", container.ExecResult{ExitCode: 0}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	intent := engine.NodeIntent{
		Path: "cancelled",
		Node: &ir.CodeStep{ID: "cancelled", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./irrelevant.sh",
		},
	}
	dr, _, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable on ctx cancel", dr.Outcome)
	}
}

func TestLocalDispatcherWithItemHandle(t *testing.T) {
	// Setup: a LocalDispatcher with two Handles entries.
	original := &engine.LocalDispatcher{
		Backend: container.NewFake(),
		Handles: map[string]container.Handle{
			"workspace": {Name: "workspace", ID: "ws-0"},
			"lab":       {Name: "lab", ID: "lab-0"},
		},
	}
	// Per-item override: "workspace" maps to a different handle.
	itemHandle := container.Handle{Name: "workspace", ID: "ws-item-3"}
	clone := original.WithItemHandle("workspace", itemHandle)

	// Clone has the override.
	if got := clone.Handles["workspace"]; got.ID != "ws-item-3" {
		t.Errorf("clone.Handles[\"workspace\"].ID = %q, want \"ws-item-3\"", got.ID)
	}
	// Clone preserves the other entry.
	if got := clone.Handles["lab"]; got.ID != "lab-0" {
		t.Errorf("clone.Handles[\"lab\"].ID = %q, want \"lab-0\"", got.ID)
	}
	// Original UNTOUCHED — the clone must not mutate the source map.
	if got := original.Handles["workspace"]; got.ID != "ws-0" {
		t.Errorf("original.Handles[\"workspace\"].ID = %q after clone, want \"ws-0\" (clone mutated source!)", got.ID)
	}
	// Backend is the same reference (shallow — both dispatchers Exec against the same fake).
	if clone.Backend != original.Backend {
		t.Error("clone.Backend != original.Backend; WithItemHandle should share the Backend")
	}
}

func TestLocalDispatcherWithItemHandleNewEntry(t *testing.T) {
	// If the name doesn't already exist in Handles, WithItemHandle adds it.
	original := &engine.LocalDispatcher{
		Backend: container.NewFake(),
		Handles: map[string]container.Handle{
			"workspace": {Name: "workspace", ID: "ws-0"},
		},
	}
	itemHandle := container.Handle{Name: "scratch", ID: "scratch-0"}
	clone := original.WithItemHandle("scratch", itemHandle)

	if got := clone.Handles["scratch"]; got.ID != "scratch-0" {
		t.Errorf("clone.Handles[\"scratch\"].ID = %q, want \"scratch-0\"", got.ID)
	}
	if _, ok := original.Handles["scratch"]; ok {
		t.Errorf("original.Handles got \"scratch\" entry; WithItemHandle should not mutate source")
	}
}

func TestLocalDispatcherInjectsAWFOutputWhenOutputSchemaDeclared(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./produce.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"verified":true}`),
	}, nil)

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified"},
		"properties":           map[string]any{"verified": map[string]any{"type": "boolean"}},
	}
	intent := engine.NodeIntent{
		Path: "verify_exploit",
		Node: &ir.CodeStep{ID: "verify_exploit", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./produce.sh",
			OutputSchema: &schema,
		},
	}
	_, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fake.Calls))
	}
	got := fake.Calls[0].Env["AWF_OUTPUT"]
	want := "/tmp/awf/verify_exploit.json"
	if got != want {
		t.Errorf("Backend.Exec received AWF_OUTPUT = %q, want %q", got, want)
	}
}

func TestLocalDispatcherSkipsAWFOutputWhenNoOutputSchema(t *testing.T) {
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./void.sh", container.ExecResult{ExitCode: 0}, nil)
	intent := engine.NodeIntent{
		Path: "void",
		Node: &ir.CodeStep{ID: "void", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./void.sh",
			// OutputSchema deliberately absent.
		},
	}
	_, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, present := fake.Calls[0].Env["AWF_OUTPUT"]; present {
		t.Errorf("AWF_OUTPUT was injected without output_schema; want absent")
	}
}

// TestLocalDispatcherDockerPathCapturesAWFOutputTempfile exercises the
// Design Q1 "Docker path" branch of runCode using the fake: Backend leaves
// ExecResult.AWFOutput nil; output_schema is declared; exit==0. The
// dispatcher must add the AWF_OUTPUT path to filesToCapture, capture it via
// the Backend, and read the bytes back for ValidateAgainstSchema.
//
// The fake doesn't natively honor AWF_OUTPUT (it ignores Env), so we
// pre-populate the in-mem fs at the expected tempfile path via WriteFile.
// This proves the dispatcher's branch works without modifying the fake.
func TestLocalDispatcherDockerPathCapturesAWFOutputTempfile(t *testing.T) {
	d, fake, h := newDispatcher(t)
	// Program Exec to return a successful exit but no AWFOutput — this is
	// the Docker backend's contract per Design Q1.
	fake.ProgramExec("./produce.sh", container.ExecResult{ExitCode: 0}, nil)
	// Pre-populate the AWF_OUTPUT tempfile path the dispatcher will compute
	// from intent.Path ("verify_dock") — see engine/awf_output.go format.
	if err := fake.WriteFile(h, "/tmp/awf/verify_dock.json", []byte(`{"verified":true,"detections":5}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified", "detections"},
		"properties": map[string]any{
			"verified":   map[string]any{"type": "boolean"},
			"detections": map[string]any{"type": "integer"},
		},
	}
	intent := engine.NodeIntent{
		Path: "verify_dock",
		Node: &ir.CodeStep{ID: "verify_dock", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./produce.sh",
			OutputSchema: &schema,
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v (err = %v), want ok", dr.Outcome, dr.Err)
	}
	if dr.Outputs["verified"] != true {
		t.Errorf("Outputs.verified = %v, want true (captured from tempfile)", dr.Outputs["verified"])
	}
	if dr.Outputs["detections"] != float64(5) {
		t.Errorf("Outputs.detections = %v, want 5 (captured from tempfile)", dr.Outputs["detections"])
	}
	// The AWF_OUTPUT tempfile must be stripped from the user-visible Files
	// slice. With no user output_files declared, dr.Files should be empty.
	if len(dr.Files) != 0 {
		t.Errorf("Files = %+v, want empty (AWF_OUTPUT must be stripped)", dr.Files)
	}
}

func TestLocalDispatcherFakePathStillUsesExecAWFOutput(t *testing.T) {
	// Regression test for Design Q1: when the Backend supplies AWFOutput
	// (Phase 2 fake path), the dispatcher MUST use it directly, NOT capture
	// the tempfile. Verifies by inspecting the typed outputs.
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("./fake-source.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"value":42}`),
	}, nil)
	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"value"},
		"properties":           map[string]any{"value": map[string]any{"type": "integer"}},
	}
	intent := engine.NodeIntent{
		Path: "fake_source",
		Node: &ir.CodeStep{ID: "fake_source", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "./fake-source.sh",
			OutputSchema: &schema,
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outputs["value"] != float64(42) {
		t.Errorf("Outputs.value = %v, want 42 (fake's ExecResult.AWFOutput is the source of truth on the fake path)", dr.Outputs["value"])
	}
	// The dispatcher MUST NOT have tried to CaptureFiles the AWF_OUTPUT
	// tempfile — the fake's CaptureFiles would have errored "path not
	// present" and the test would have classified retryable_failure.
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok (dispatcher should NOT capture tempfile on the fake path)", dr.Outcome)
	}
}

func TestSplitContainerRefNoColon(t *testing.T) {
	bare, svc := engine.SplitContainerRef("lab")
	if bare != "lab" || svc != "" {
		t.Errorf("SplitContainerRef(\"lab\") = (%q, %q), want (\"lab\", \"\")", bare, svc)
	}
}

func TestSplitContainerRefWithColon(t *testing.T) {
	bare, svc := engine.SplitContainerRef("lab:db")
	if bare != "lab" || svc != "db" {
		t.Errorf("SplitContainerRef(\"lab:db\") = (%q, %q), want (\"lab\", \"db\")", bare, svc)
	}
}

func TestSplitContainerRefMultipleColons(t *testing.T) {
	// "lab:db:replica" → ("lab", "db:replica"). The Backend may further
	// split or reject; the dispatcher only splits the first colon.
	bare, svc := engine.SplitContainerRef("lab:db:replica")
	if bare != "lab" || svc != "db:replica" {
		t.Errorf("SplitContainerRef(\"lab:db:replica\") = (%q, %q), want (\"lab\", \"db:replica\")", bare, svc)
	}
}

func TestResolvedInputs_AgentFields_ZeroValueByDefault(t *testing.T) {
	// Default-constructed ResolvedInputs has nil Uses/With —
	// existing code-step paths must remain unaffected by slice 5.2's
	// additive extension.
	var ri engine.ResolvedInputs
	if ri.Uses != "" {
		t.Errorf("Uses default = %q, want empty", ri.Uses)
	}
	if ri.With != nil {
		t.Errorf("With default = %v, want nil", ri.With)
	}
}

func TestDispatchResult_AgentEventsField_ZeroValueByDefault(t *testing.T) {
	var dr engine.DispatchResult
	if dr.AgentEvents != nil {
		t.Errorf("AgentEvents default = %v, want nil", dr.AgentEvents)
	}
}

func TestLocalDispatcher_AgentFields_ZeroValueOK(t *testing.T) {
	// Phase 4 callers construct LocalDispatcher{Backend: ..., Handles: ..., ComposeFiles: ...}
	// without setting Resolver / AgentEventTap. Those fields must default to
	// nil/empty values and not break code-step dispatch.
	d := &engine.LocalDispatcher{}
	if d.Resolver != nil {
		t.Errorf("Resolver default = %v, want nil", d.Resolver)
	}
	if d.AgentEventTap != nil {
		t.Errorf("AgentEventTap default = %v, want nil", d.AgentEventTap)
	}
}

func TestResolvedInputs_AgentFields_Populated(t *testing.T) {
	ri := engine.ResolvedInputs{
		Uses: "anthropic/claude-code",
		With: ir.RawConfig{"prompt": "do the thing"},
	}
	if ri.Uses != "anthropic/claude-code" {
		t.Errorf("Uses = %q", ri.Uses)
	}
	if ri.With["prompt"] != "do the thing" {
		t.Errorf("With[prompt] = %v", ri.With["prompt"])
	}
}
