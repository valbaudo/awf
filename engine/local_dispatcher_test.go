package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
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

func TestRunCodeStagesInputFiles(t *testing.T) {
	// SP1 artifact channel: the dispatcher calls Backend.CopyTo to stage
	// ResolvedInputs.InputFiles into the container BEFORE Exec. This is the
	// byte-exact staging proof (the conformance bucket proves the wiring).
	d, fake, h := newDispatcher(t)
	fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
	intent := engine.NodeIntent{
		Path: "consume",
		Node: &ir.CodeStep{ID: "consume", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./consume.sh",
			InputFiles: []container.InputFile{
				{Path: "/work/r.md", Content: []byte("R")},
			},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	got, err := fake.CaptureFiles(context.Background(), h, []string{"/work/r.md"})
	if err != nil {
		t.Fatalf("CaptureFiles after staging: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != "R" {
		t.Errorf("staged file = %+v, want one file /work/r.md content %q", got, "R")
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

func TestLocalDispatcherMissingAWFOutputNamesStepAndVar(t *testing.T) {
	// A code step declares output_schema, exits 0, but never writes $AWF_OUTPUT.
	// The error must mention the step path AND "$AWF_OUTPUT" so the author
	// knows exactly which step and which env var to check.
	d, fake, _ := newDispatcher(t)
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0, AWFOutput: nil}, nil)
	schema := ir.JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "string"},
		},
	}
	intent := engine.NodeIntent{
		Path: "recon",
		Node: &ir.CodeStep{ID: "recon", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      "true",
			OutputSchema: &schema,
			// OutputFiles intentionally absent — only AWF_OUTPUT tempfile is captured.
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable_failure", dr.Outcome)
	}
	if dr.Err == nil {
		t.Fatal("dr.Err is nil; want enriched capture error")
	}
	if !strings.Contains(dr.Err.Error(), "recon") {
		t.Errorf("dr.Err = %q; want it to contain step path %q", dr.Err.Error(), "recon")
	}
	if !strings.Contains(dr.Err.Error(), "AWF_OUTPUT") {
		t.Errorf("dr.Err = %q; want it to contain \"AWF_OUTPUT\"", dr.Err.Error())
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

func TestLocalDispatcher_AgentStep_NoLongerUnsupportedKind(t *testing.T) {
	// Pre-slice-5.2: AgentStep dispatch returned engine.ErrUnsupportedKind.
	// Slice 5.2 routes AgentStep through runAgent. With no Resolver set,
	// runAgent returns *agent.ErrAdapterNotFound (which is NOT
	// ErrUnsupportedKind). This test pins the routing change.
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &agent.Registry{}, // empty — no adapter registered
	}
	intent := engine.NodeIntent{
		Path: "graph[0]",
		Node: &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses: "anthropic/claude-code",
		},
	}
	_, _, err := d.Run(context.Background(), intent)
	if errors.Is(err, engine.ErrUnsupportedKind) {
		t.Fatalf("err = ErrUnsupportedKind; want not (Task 4 should have removed AgentStep from that arm)")
	}
	var notFound *agent.ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v (%T); want *agent.ErrAdapterNotFound", err, err)
	}
	if notFound.Ref != "anthropic/claude-code" {
		t.Errorf("ErrAdapterNotFound.Ref = %q, want %q", notFound.Ref, "anthropic/claude-code")
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

// rejectingAdapter overrides fake.Fake's ValidateConfig + injects a custom
// Launch error for the ErrInvalidConfig + ErrAgentLaunch branches.
type rejectingAdapter struct {
	*fake.Fake
	validateErr error
	launchErr   error
}

func (r *rejectingAdapter) ValidateConfig(_ ir.RawConfig) error { return r.validateErr }

func (r *rejectingAdapter) Launch(_ context.Context, _ container.Handle, _ agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if r.launchErr != nil {
		return nil, nil, r.launchErr
	}
	return nil, nil, fmt.Errorf("test stub: missing happy-path script")
}

func TestLocalDispatcher_runAgent_AdapterNotFound(t *testing.T) {
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &agent.Registry{}, // empty
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "x", Container: "lab", Uses: "missing/adapter"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "missing/adapter"},
	}
	_, _, err := d.Run(context.Background(), intent)
	var notFound *agent.ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want *agent.ErrAdapterNotFound", err)
	}
	if notFound.Ref != "missing/adapter" {
		t.Errorf("Ref = %q, want %q", notFound.Ref, "missing/adapter")
	}
}

func TestLocalDispatcher_runAgent_ValidateConfigRejectsUnknownKey(t *testing.T) {
	var reg agent.Registry
	adapter := &rejectingAdapter{
		Fake:        fake.New("anthropic/claude-code"),
		validateErr: &agent.ErrInvalidConfig{Ref: "anthropic/claude-code", Key: "session_id", Reason: "session reuse is forbidden"},
	}
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	intent := engine.NodeIntent{
		Path: "graph[0]",
		Node: &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses: "anthropic/claude-code",
			With: ir.RawConfig{"prompt": "p", "session_id": "should-be-rejected"},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %q, want %q", dr.Outcome, engine.OutcomePermanentFailure)
	}
	var badConfig *agent.ErrInvalidConfig
	if !errors.As(dr.Err, &badConfig) {
		t.Fatalf("dr.Err = %v, want *agent.ErrInvalidConfig", dr.Err)
	}
}

func TestLocalDispatcher_runAgent_AgentLaunchError(t *testing.T) {
	var reg agent.Registry
	transport := &agent.ErrAgentLaunch{Cause: errors.New("docker exec: i/o timeout")}
	adapter := &rejectingAdapter{
		Fake:      fake.New("anthropic/claude-code"),
		launchErr: transport,
	}
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "anthropic/claude-code", With: ir.RawConfig{"prompt": "p"}},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %q, want %q (transport class)", dr.Outcome, engine.OutcomeRetryableFailure)
	}
	if !errors.Is(dr.Err, transport.Cause) {
		t.Fatalf("dr.Err does not wrap transport cause: %v", dr.Err)
	}
}

func TestLocalDispatcher_runAgent_OutputSchemaMismatch(t *testing.T) {
	var reg agent.Registry
	// Schema requires {verdict: bool}; the fake returns {verdict: "string"} which violates.
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"verdict": "not-a-bool"},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	// ir.JSONSchema is map[string]any — build via map literal.
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"verdict"},
		"properties": map[string]any{
			"verdict": map[string]any{"type": "boolean"},
		},
	}
	intent := engine.NodeIntent{
		Path: "graph[0]",
		Node: &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:         "anthropic/claude-code",
			With:         ir.RawConfig{"prompt": "p"},
			OutputSchema: schema,
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %q, want %q (unparseable output)", dr.Outcome, engine.OutcomeRetryableFailure)
	}
	var unparseable *agent.ErrUnparseableOutput
	if !errors.As(dr.Err, &unparseable) {
		t.Fatalf("dr.Err = %v, want *agent.ErrUnparseableOutput", dr.Err)
	}
}

func TestLocalDispatcher_runAgent_AgentEventsBufferedAndTapped(t *testing.T) {
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{
			{Kind: "system", Payload: []byte(`{"subtype":"init","session_id":"abc"}`)},
			{Kind: "assistant", Payload: []byte(`{"text":"thinking..."}`)},
			{Kind: "result", Payload: []byte(`{"subtype":"success","total_cost_usd":0.012}`)},
		},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var tapBuf strings.Builder
	d := &engine.LocalDispatcher{
		Backend:       container.NewFake(),
		Handles:       map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver:      &reg,
		AgentEventTap: &tapBuf,
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "anthropic/claude-code", With: ir.RawConfig{"prompt": "p"}},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Buffered events: all 3.
	if len(dr.AgentEvents) != 3 {
		t.Fatalf("len(AgentEvents) = %d, want 3", len(dr.AgentEvents))
	}
	wantKinds := []string{"system", "assistant", "result"}
	for i, want := range wantKinds {
		if dr.AgentEvents[i].Kind != want {
			t.Errorf("AgentEvents[%d].Kind = %q, want %q", i, dr.AgentEvents[i].Kind, want)
		}
	}

	// Live tap: one line per event, [<kind>] prefix.
	tap := tapBuf.String()
	for _, kind := range wantKinds {
		needle := "[" + kind + "]"
		if !strings.Contains(tap, needle) {
			t.Errorf("tap output missing %q\nfull tap:\n%s", needle, tap)
		}
	}
	// Newline-terminated lines (test the printer's basic shape).
	if !strings.HasSuffix(tap, "\n") {
		t.Errorf("tap output not newline-terminated: %q", tap)
	}
}

func TestLocalDispatcher_runAgent_NilAgentEventTap_NoBuffering(t *testing.T) {
	// Production CLI may not always wire a tap (--quiet, batch mode). The
	// dispatcher must still buffer AgentEvents for the interpreter's
	// log-writing pass, but skip the tap writes.
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{{Kind: "result", Payload: []byte(`{}`)}},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
		// AgentEventTap: nil
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "anthropic/claude-code"},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(dr.AgentEvents) != 1 {
		t.Errorf("len(AgentEvents) = %d, want 1 (buffer preserved even with nil tap)", len(dr.AgentEvents))
	}
}

func TestLocalDispatcher_runAgent_HappyPath(t *testing.T) {
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"verdict": "pass", "confidence": 0.95},
		Cost:   0.012,
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	// ir.JSONSchema is map[string]any (ir/types.go:94 — verified). Build via
	// map literal, not struct literal — there are no typed fields.
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"verdict", "confidence"},
		"properties": map[string]any{
			"verdict":    map[string]any{"type": "string"},
			"confidence": map[string]any{"type": "number"},
		},
	}
	intent := engine.NodeIntent{
		Path: "graph[0]",
		Node: &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:         "anthropic/claude-code",
			With:         ir.RawConfig{"prompt": "do the thing"},
			OutputSchema: schema,
		},
	}
	dr, chunks, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err=%v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}
	if dr.Outputs["verdict"] != "pass" {
		t.Errorf("Outputs[verdict] = %v, want %q", dr.Outputs["verdict"], "pass")
	}
	// chunks channel must be non-nil and drained (the adapter contract — Launch
	// returns synchronously AFTER its own channel closes; the dispatcher
	// returns chunks closed too).
	if chunks == nil {
		t.Errorf("chunks = nil; want non-nil (callers must be able to range over it)")
	} else {
		for range chunks {
			// drain
		}
	}
	// fake.Calls() must record the invocation with the With map intact (so
	// slice 5.2 wiring is provably end-to-end).
	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake.Calls() len = %d, want 1", len(calls))
	}
	if got := calls[0].With["prompt"]; got != "do the thing" {
		t.Errorf("With[prompt] = %v, want %q", got, "do the thing")
	}
}

// TestLocalDispatcher_runCode_StreamingDrain pins the slice 5.3 drain-to-slice
// shape of LocalDispatcher.runCode: Backend.Exec now returns
// (chunks, result, error); runCode drains chunks before reading result, then
// re-emits collected chunks on a pre-closed channel for the interpreter's
// drainTap. Live-tap for CodeSteps is post-hoc by design (forwarder-goroutine
// pattern deadlocks on long streams — see runCode's inline rationale).
func TestLocalDispatcher_runCode_StreamingDrain(t *testing.T) {
	f := container.NewFake()
	h, err := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.ProgramExec("echo hi", container.ExecResult{ExitCode: 0, Stdout: []byte("hi\n")}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("hi\n")},
	})
	d := &engine.LocalDispatcher{
		Backend: f,
		Handles: map[string]container.Handle{"lab": h},
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.CodeStep{ID: "x", Container: "lab", Run: "echo hi"},
		ResolvedInputs: engine.ResolvedInputs{Command: "echo hi"},
	}
	dr, chunks, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %q, want %q", dr.Outcome, engine.OutcomeOK)
	}
	// Drain the dispatcher's returned chunks channel — interpreter does this via drainTap.
	var seen []container.IOChunk
	for c := range chunks {
		seen = append(seen, c)
	}
	if len(seen) != 1 || string(seen[0].Data) != "hi\n" {
		t.Errorf("chunks = %+v", seen)
	}
	if string(dr.Stdout) != "hi\n" {
		t.Errorf("Stdout = %q, want hi\\n", dr.Stdout)
	}
}

func TestLocalDispatcher_runAgent_StreamsEventsProgressively(t *testing.T) {
	// Asserts the tap renders events as they arrive (NOT all at once after
	// outcome). Uses agent/fake.WithEmitDelay so emissions are paced.
	const delay = 50 * time.Millisecond

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		WithEmitDelay(delay).
		Script(0, fake.Result{
			Output: map[string]any{"ok": true},
			Events: []agent.AgentEvent{
				{Kind: "a"}, {Kind: "b"}, {Kind: "c"},
			},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tap := newTimestampTap()
	d := &engine.LocalDispatcher{
		Backend:       container.NewFake(),
		Handles:       map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver:      &reg,
		AgentEventTap: tap,
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "x", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "anthropic/claude-code", With: ir.RawConfig{"prompt": "p"}},
	}

	start := time.Now()
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = start // for clarity; the tap captures wall-clock per write

	writes := tap.Writes()
	if len(writes) != 3 {
		t.Fatalf("tap writes = %d, want 3", len(writes))
	}
	span := writes[2].At.Sub(writes[0].At)
	if span < delay {
		t.Errorf("first→last tap-write span = %v, want ≥ %v (proves tap renders progressively, not buffer-then-burst)", span, delay)
	}

	// Assert dr.AgentEvents was populated for downstream log writes.
	// A regression that dropped events into the buffer would fan-out break
	// silently — the kinds-in-order check pins it.
	if len(dr.AgentEvents) != 3 {
		t.Errorf("dr.AgentEvents len = %d, want 3 (events buffered for interpreter agent.event log writes)", len(dr.AgentEvents))
	}
	wantKinds := []string{"a", "b", "c"}
	for i, want := range wantKinds {
		if i >= len(dr.AgentEvents) {
			break
		}
		if dr.AgentEvents[i].Kind != want {
			t.Errorf("dr.AgentEvents[%d].Kind = %q, want %q", i, dr.AgentEvents[i].Kind, want)
		}
	}
}

// timestampTap records the wall-clock time of each Write.
type timestampTap struct {
	mu sync.Mutex
	w  []timestampedWrite
}
type timestampedWrite struct {
	Data []byte
	At   time.Time
}

func newTimestampTap() *timestampTap { return &timestampTap{} }
func (t *timestampTap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dup := make([]byte, len(p))
	copy(dup, p)
	t.w = append(t.w, timestampedWrite{Data: dup, At: time.Now()})
	return len(p), nil
}
func (t *timestampTap) Writes() []timestampedWrite {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]timestampedWrite, len(t.w))
	copy(out, t.w)
	return out
}

func TestResolvedInputs_FeedbackField_ZeroValueByDefault(t *testing.T) {
	var ri engine.ResolvedInputs
	if ri.Feedback != nil {
		t.Errorf("Feedback default = %v, want nil", ri.Feedback)
	}
}

func TestResolvedInputs_FeedbackField_Populated(t *testing.T) {
	ri := engine.ResolvedInputs{
		Feedback: ir.RawConfig{"verified": false, "feedback": "missing detection"},
	}
	if ri.Feedback["verified"] != false {
		t.Errorf("Feedback[verified] = %v, want false", ri.Feedback["verified"])
	}
	if ri.Feedback["feedback"] != "missing detection" {
		t.Errorf("Feedback[feedback] = %v, want %q", ri.Feedback["feedback"], "missing detection")
	}
}

func TestLocalDispatcher_runAgent_ThreadFeedback(t *testing.T) {
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"done": true},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	intent := engine.NodeIntent{
		Path: "gate[0].attempt-2.generate[0]",
		Node: &ir.AgentStep{ID: "gen", Container: "lab", Uses: "anthropic/claude-code"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:     "anthropic/claude-code",
			With:     ir.RawConfig{"prompt": "do the thing"},
			Feedback: ir.RawConfig{"verified": false, "feedback": "missing detection"},
		},
	}
	dr, _, err := d.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", dr.Outcome)
	}
	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls len = %d", len(calls))
	}
	if calls[0].Feedback == nil {
		t.Fatal("Feedback = nil; want propagated from ResolvedInputs")
	}
	if calls[0].Feedback["feedback"] != "missing detection" {
		t.Errorf("Feedback[feedback] = %v", calls[0].Feedback["feedback"])
	}
}

func TestLocalDispatcher_runAgent_StepCostLine(t *testing.T) {
	ctx := context.Background()
	cfake := container.NewFake()
	h, err := cfake.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fk := fake.New("test/agent").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Cost:   0.0123,
		Tokens: agent.MetricTokens{Input: 100, Output: 50},
	})
	reg := &agent.Registry{}
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var tapBuf bytes.Buffer
	disp := &engine.LocalDispatcher{
		Backend: cfake, Handles: map[string]container.Handle{"lab": h}, Resolver: reg,
		AgentEventTap: &tapBuf,
		StepCostLine:  true,
	}
	intent := engine.NodeIntent{
		Path:           "a1",
		Node:           &ir.AgentStep{ID: "a1", Container: "lab", Uses: "test/agent"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "test/agent"},
	}
	if _, _, err := disp.Run(ctx, intent); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tap := tapBuf.String()
	if !strings.Contains(tap, "a1") || !strings.Contains(tap, "0.0123") || !strings.Contains(tap, "turns") {
		t.Errorf("tap missing per-step cost line:\n%s", tap)
	}
}

func TestLocalDispatcher_runAgent_StepCostLineOffByDefault(t *testing.T) {
	ctx := context.Background()
	cfake := container.NewFake()
	h, _ := cfake.Create(ctx, container.ContainerSpec{Name: "lab"})
	fk := fake.New("test/agent").Script(0, fake.Result{Output: map[string]any{"ok": true}, Cost: 0.5})
	reg := &agent.Registry{}
	_ = reg.Register(fk)
	var tapBuf bytes.Buffer
	disp := &engine.LocalDispatcher{Backend: cfake, Handles: map[string]container.Handle{"lab": h}, Resolver: reg, AgentEventTap: &tapBuf}
	intent := engine.NodeIntent{Path: "a1", Node: &ir.AgentStep{ID: "a1", Container: "lab", Uses: "test/agent"}, ResolvedInputs: engine.ResolvedInputs{Uses: "test/agent"}}
	if _, _, err := disp.Run(ctx, intent); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(tapBuf.String(), "turns") {
		t.Errorf("cost line emitted with StepCostLine unset:\n%s", tapBuf.String())
	}
}

func TestDispatcherCapturesSnapshotOnEligibleOkStep(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	fake := container.NewFake().WithBlobs(blobs)
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	h, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	d := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"ws": h}}
	intent := engine.NodeIntent{
		Path: "s1", Node: &ir.CodeStep{ID: "s1", Container: "ws", Run: "true"},
		ResolvedInputs: engine.ResolvedInputs{Command: "true", Env: map[string]string{}, Snapshot: "workspace"},
	}
	dr, ch, err := d.Run(context.Background(), intent)
	for range ch {
	}
	if err != nil || dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Run: outcome=%q err=%v", dr.Outcome, err)
	}
	if dr.SnapshotRef == "" || dr.Container != "ws" {
		t.Errorf("dr = {ref:%q, container:%q}, want non-empty ref + ws", dr.SnapshotRef, dr.Container)
	}

	// Non-eligible step must NOT capture.
	intent.ResolvedInputs.Snapshot = ""
	dr2, ch2, _ := d.Run(context.Background(), intent)
	for range ch2 {
	}
	if dr2.SnapshotRef != "" {
		t.Errorf("non-eligible SnapshotRef = %q, want empty", dr2.SnapshotRef)
	}
}

func TestDispatcherSnapshotTerminalErrorIsPermanent(t *testing.T) {
	// A backend that cannot snapshot (no blobs wired → Snapshot returns
	// container.ErrUnsupported, a terminal condition) yields permanent_failure,
	// not retryable — retrying would re-run the whole step to fail identically.
	fake := container.NewFake() // NO WithBlobs ⇒ Snapshot returns ErrUnsupported
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	h, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	d := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"ws": h}}
	intent := engine.NodeIntent{
		Path: "s1", Node: &ir.CodeStep{ID: "s1", Container: "ws", Run: "true"},
		ResolvedInputs: engine.ResolvedInputs{Command: "true", Env: map[string]string{}, Snapshot: "workspace"},
	}
	dr, ch, _ := d.Run(context.Background(), intent)
	for range ch {
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Errorf("terminal snapshot error: outcome=%q, want permanent_failure", dr.Outcome)
	}
}
