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
	d, fake, _ := newDispatcher(t)
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
	_ = fake
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
