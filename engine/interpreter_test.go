package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// refExpr returns a *ir.Expr pointer to the given string — slim sugar for IR
// construction in tests (ir.Loop.Until is *ir.Expr, omitempty).
func refExpr(s string) *ir.Expr {
	e := ir.Expr(s)
	return &e
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newRunHarness builds the in-mem fakes + a default RunState seeded with
// run.started + a single-container handle map.
func newRunHarness(t *testing.T) (*container.Fake, container.Handle, *engine.LocalDispatcher, *state.InMemoryLog, *state.InMemoryBlobs, *clock.Fake, *engine.RunState) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disp := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": h},
	}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", nil)
	return fake, h, disp, log, blobs, clk, rs
}

func TestRunEmptyGraphIsOK(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			t.Errorf("unexpected node.completed event on empty graph: %+v", e)
		}
	}
}

func TestRunSingleCodeStepHappyPath(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./hello.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("hello\n"),
	}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "hello", Container: "lab", Run: "./hello.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	var tap bytes.Buffer
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{Tap: &tap})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.Completed["hello"]
	if !ok {
		t.Fatal("RunState.Completed missing 'hello'")
	}
	if nr.Outcome != engine.OutcomeOK || string(nr.Stdout) != "hello\n" {
		t.Errorf("nr = %+v, want ok + stdout 'hello'", nr)
	}
	events, _ := log.Fold()
	var sawCompleted bool
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == "hello" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("no node.completed event for 'hello'; events: %+v", events)
	}
}

func TestRunSequentialCodeStepsResolveCrossStepRefs(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step1.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"greeting":"hello"}`),
		Stdout:    []byte("step1\n"),
	}, nil)
	fake.ProgramExec("./step2.sh hello", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("step2\n"),
	}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID: "step1", Container: "lab", Run: "./step1.sh",
			OutputSchema: &ir.JSONSchema{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"greeting"},
				"properties":           map[string]any{"greeting": map[string]any{"type": "string"}},
			},
		},
		&ir.CodeStep{ID: "step2", Container: "lab", Run: "./step2.sh {{ step.step1.greeting }}"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	if _, ok := rs.Completed["step1"]; !ok {
		t.Error("RunState.Completed missing step1")
	}
	if _, ok := rs.Completed["step2"]; !ok {
		t.Error("RunState.Completed missing step2")
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("fake.Calls len = %d, want 2", len(fake.Calls))
	}
	if fake.Calls[1].Run != "./step2.sh hello" {
		t.Errorf("step2 command = %q, want substituted %q", fake.Calls[1].Run, "./step2.sh hello")
	}
}

func TestRunCodeStepIdempotencyKeySubstituted(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"cve_id": "CVE-2024-9999"}
	fake.ProgramExec("./open-pr.sh", container.ExecResult{ExitCode: 0}, nil)

	idemTpl := ir.Template("{{ input.cve_id }}:pr")
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID:             "open_pr",
			Container:      "lab",
			Run:            "./open-pr.sh",
			IdempotencyKey: &idemTpl,
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fake.Calls[0].Env["AWF_IDEMPOTENCY_KEY"]
	if got != "CVE-2024-9999:pr" {
		t.Errorf("AWF_IDEMPOTENCY_KEY = %q, want %q", got, "CVE-2024-9999:pr")
	}
}

// TestRun_CodeStepReceivesWorkflowEnv is F15: a code step's dispatched env
// includes a forwarded workflow env: value when RunOptions.RunEnv is set. The
// engine only copies the CLI-resolved map — see TestRunCodeStepDoesNotLeakRunEnvAcrossSteps
// below for the no-aliasing guarantee.
func TestRun_CodeStepReceivesWorkflowEnv(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./use-var.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{
		Env: []string{"MY_VAR"},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "use_var", Container: "lab", Run: "./use-var.sh"},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	opts := engine.RunOptions{RunEnv: map[string]string{"MY_VAR": "hello"}}
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fake.Calls))
	}
	got := fake.Calls[0].Env["MY_VAR"]
	if got != "hello" {
		t.Errorf("Backend.Exec received MY_VAR = %q, want %q", got, "hello")
	}
}

// TestRunCodeStepDoesNotLeakRunEnvAcrossSteps is the additive/no-aliasing half
// of F15: RunOptions.RunEnv is nil (pre-F15 behavior) — a code step's
// dispatched env must stay empty, exactly like before RunEnv existed.
func TestRunCodeStepDoesNotLeakRunEnvAcrossSteps(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./no-env.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "no_env", Container: "lab", Run: "./no-env.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.Calls[0].Env) != 0 {
		t.Errorf("fake.Calls[0].Env = %+v, want empty (RunOptions.RunEnv unset)", fake.Calls[0].Env)
	}
}

func TestRunCodeStepFailureAppendsNodeFailed(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./misconfig.sh", container.ExecResult{ExitCode: 78}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "misconfig", Container: "lab", Run: "./misconfig.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil {
		t.Error("err is nil; want underlying failure cause")
	}
	if _, ok := rs.Completed["misconfig"]; ok {
		t.Error("RunState.Completed has 'misconfig' — failed steps must NOT commit")
	}
	events, _ := log.Fold()
	var failedFound bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "misconfig" {
			failedFound = true
		}
	}
	if !failedFound {
		t.Errorf("no node.failed event for 'misconfig'; events: %+v", events)
	}
}

func TestRunInputFilesCrossContainerHandoff(t *testing.T) {
	// SP1 artifact channel end-to-end: step A (in container `lab`) produces a
	// NAMED output_files artifact; step B (in a DISTINCT container `box`)
	// input_files it. The interpreter resolves the committed CAS ref, Blobs.Get's
	// the bytes, and the dispatcher CopyTo's them into B's container before B runs.
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	labH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	boxH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}
	disp := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": labH, "box": boxH},
	}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", nil)

	// The producer's exec produces /out/report.md (seeded directly on lab's
	// handle so the post-Exec CaptureFiles finds it). This engine test holds the
	// handle directly, so WriteFile is the natural seed; the conformance bucket,
	// which can't reach the harness-internal handle, uses ProgramExecWithFiles.
	sentinel := []byte("recon findings\n")
	fake.ProgramExec("./recon.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(labH, "/out/report.md", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake.ProgramExec("./hunt.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}, "box": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "recon", Container: "lab", Run: "./recon.sh",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
			},
			&ir.CodeStep{
				ID: "hunt", Container: "box", Run: "./hunt.sh",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	if _, ok := rs.Completed["hunt"]; !ok {
		t.Fatal("RunState.Completed missing 'hunt'")
	}
	// The sentinel was seeded ONLY into lab; hunt runs in box. The staged bytes
	// landing in box at /work/report.md proves the full resolve→Get→CopyTo path.
	got, err := fake.CaptureFiles(context.Background(), boxH, []string{"/work/report.md"})
	if err != nil {
		t.Fatalf("CaptureFiles box /work/report.md: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(sentinel) {
		t.Errorf("staged into box = %+v, want one file with content %q", got, sentinel)
	}
}

func TestRunInputFilesStagesFileAssetFromRunSnapshot(t *testing.T) {
	t.Parallel()
	fake, h, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
	ref, err := blobs.Put([]byte("run-start bytes\n"))
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		ID: "asset-file", Version: 1,
		Assets:     map[string]string{"prompt": "prompt.txt"},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "consume", Container: "lab", Run: "./consume.sh",
				InputFiles: map[string]string{"/work/prompt.txt": "asset.prompt"},
			},
		},
	}
	asset := engine.RunStartedAsset{
		DeclaredPath: "prompt.txt",
		Files: []engine.RunStartedAssetFile{{
			Path: ".", Ref: ref, Size: int64(len("run-start bytes\n")), SHA256: sha256Hex([]byte("run-start bytes\n")),
		}},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{
		Assets: map[string]engine.RunStartedAsset{"prompt": asset},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	got, err := fake.CaptureFiles(context.Background(), h, []string{"/work/prompt.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if string(got[0].Content) != "run-start bytes\n" {
		t.Fatalf("staged bytes = %q", got[0].Content)
	}
}

func TestRunInputFilesStagesDirectoryAssetFromRunSnapshot(t *testing.T) {
	t.Parallel()
	fake, h, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
	refA, err := blobs.Put([]byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	refB, err := blobs.Put([]byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		ID: "asset-dir", Version: 1,
		Assets:     map[string]string{"fixtures": "fixtures"},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "consume", Container: "lab", Run: "./consume.sh",
				InputFiles: map[string]string{"/work/assets": "asset.fixtures"},
			},
		},
	}
	asset := engine.RunStartedAsset{
		DeclaredPath: "fixtures",
		IsDir:        true,
		Files: []engine.RunStartedAssetFile{
			{Path: "a.txt", Ref: refA, Size: 1, SHA256: sha256Hex([]byte("a"))},
			{Path: "nested/b.txt", Ref: refB, Size: 1, SHA256: sha256Hex([]byte("b"))},
		},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{
		Assets: map[string]engine.RunStartedAsset{"fixtures": asset},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	got, err := fake.CaptureFiles(context.Background(), h, []string{"/work/assets/a.txt", "/work/assets/nested/b.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if string(got[0].Content) != "a" || string(got[1].Content) != "b" {
		t.Fatalf("staged files = %+v", got)
	}
}

func TestRunInputFilesStagesRecordedAssetNotLoadedAssetBytes(t *testing.T) {
	t.Parallel()
	fake, h, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)
	ref, err := blobs.Put([]byte("recorded"))
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		ID: "asset-recorded", Version: 1,
		Assets:     map[string]string{"input": "asset.txt"},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "consume", Container: "lab", Run: "./consume.sh",
				InputFiles: map[string]string{"/work/asset.txt": "asset.input"},
			},
		},
	}
	ld := &ir.LoadedDefinition{
		Workflow: wf,
		Assets: map[string]ir.LoadedAsset{"input": {
			ID: "input", DeclaredPath: "asset.txt",
			Files: []ir.LoadedAssetFile{{Path: ".", Bytes: []byte("current checkout")}},
		}},
	}
	asset := engine.RunStartedAsset{
		DeclaredPath: "asset.txt",
		Files: []engine.RunStartedAssetFile{{
			Path: ".", Ref: ref, Size: int64(len("recorded")), SHA256: sha256Hex([]byte("recorded")),
		}},
	}
	oc, err := engine.Run(context.Background(), ld, rs, disp, log, blobs, clk, engine.RunOptions{
		Assets: map[string]engine.RunStartedAsset{"input": asset},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	got, err := fake.CaptureFiles(context.Background(), h, []string{"/work/asset.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if string(got[0].Content) != "recorded" {
		t.Fatalf("staged bytes = %q, want recorded snapshot bytes", got[0].Content)
	}
}

func TestRunInputFilesMissingAssetBlobIsInternalBeforeExec(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./must-not-run.sh", container.ExecResult{ExitCode: 0}, nil)
	wf := &ir.Workflow{
		ID: "asset-missing", Version: 1,
		Assets:     map[string]string{"input": "asset.txt"},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "consume", Container: "lab", Run: "./must-not-run.sh",
				InputFiles: map[string]string{"/work/asset.txt": "asset.input"},
			},
		},
	}
	missing := "awf-d1:sha256:" + strings.Repeat("d", 64)
	asset := engine.RunStartedAsset{
		DeclaredPath: "asset.txt",
		Files:        []engine.RunStartedAssetFile{{Path: ".", Ref: missing, Size: 1, SHA256: strings.Repeat("0", 64)}},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{
		Assets: map[string]engine.RunStartedAsset{"input": asset},
	})
	if err == nil || !strings.Contains(err.Error(), "input_files artifact fetch failed") {
		t.Fatalf("Run err = %v, want artifact fetch failure", err)
	}
	if oc != "" {
		t.Fatalf("Outcome = %q, want internal empty outcome", oc)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("Exec calls = %+v, want none before missing asset blob failure", fake.Calls)
	}
	if _, ok := rs.Completed["consume"]; ok {
		t.Fatal("missing asset blob committed node.completed")
	}
}

func TestRunInputFilesMalformedAssetManifestIsInternalBeforeExec(t *testing.T) {
	t.Parallel()
	for name, asset := range map[string]engine.RunStartedAsset{
		"missing from run.started": {},
		"file wrong shape": {
			DeclaredPath: "asset.txt",
			Files:        []engine.RunStartedAssetFile{{Path: "not-dot"}},
		},
		"directory unsafe path": {
			DeclaredPath: "fixtures",
			IsDir:        true,
			Files:        []engine.RunStartedAssetFile{{Path: "../escape"}},
		},
		"directory empty": {
			DeclaredPath: "fixtures",
			IsDir:        true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
			fake.ProgramExec("./must-not-run.sh", container.ExecResult{ExitCode: 0}, nil)
			wf := &ir.Workflow{
				ID: "asset-malformed", Version: 1,
				Assets:     map[string]string{"input": "asset.txt"},
				Containers: map[string]ir.Container{"lab": {}},
				Graph: ir.NodeList{
					&ir.CodeStep{
						ID: "consume", Container: "lab", Run: "./must-not-run.sh",
						InputFiles: map[string]string{"/work/asset.txt": "asset.input"},
					},
				},
			}
			assets := map[string]engine.RunStartedAsset{"input": asset}
			if name == "missing from run.started" {
				assets = nil
			}
			oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{Assets: assets})
			if err == nil || !strings.Contains(err.Error(), "input_files artifact fetch failed") {
				t.Fatalf("Run err = %v, want internal asset manifest failure", err)
			}
			if oc != "" {
				t.Fatalf("Outcome = %q, want internal empty outcome", oc)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("Exec calls = %+v, want none before malformed asset manifest failure", fake.Calls)
			}
			if _, ok := rs.Completed["consume"]; ok {
				t.Fatal("malformed asset manifest committed node.completed")
			}
		})
	}
}

func TestRunInputFilesAssetExpansionRejectsPathCollisions(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		inputFiles map[string]string
		dirPath    string
	}{
		"duplicate": {
			inputFiles: map[string]string{
				"/work/assets":   "asset.fixtures",
				"/work/assets/a": "asset.single",
			},
			dirPath: "a",
		},
		"ancestor-descendant": {
			inputFiles: map[string]string{
				"/work/assets": "asset.single",
				"/work":        "asset.fixtures",
			},
			dirPath: "assets/a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
			fake.ProgramExec("./must-not-run.sh", container.ExecResult{ExitCode: 0}, nil)
			refSingle, err := blobs.Put([]byte("single"))
			if err != nil {
				t.Fatal(err)
			}
			refDir, err := blobs.Put([]byte("dir"))
			if err != nil {
				t.Fatal(err)
			}
			wf := &ir.Workflow{
				ID: "asset-collision", Version: 1,
				Assets:     map[string]string{"single": "single.txt", "fixtures": "fixtures"},
				Containers: map[string]ir.Container{"lab": {}},
				Graph: ir.NodeList{
					&ir.CodeStep{
						ID: "consume", Container: "lab", Run: "./must-not-run.sh",
						InputFiles: tc.inputFiles,
					},
				},
			}
			assets := map[string]engine.RunStartedAsset{
				"single": {
					DeclaredPath: "single.txt",
					Files: []engine.RunStartedAssetFile{{
						Path: ".", Ref: refSingle, Size: int64(len("single")), SHA256: sha256Hex([]byte("single")),
					}},
				},
				"fixtures": {
					DeclaredPath: "fixtures",
					IsDir:        true,
					Files: []engine.RunStartedAssetFile{{
						Path: tc.dirPath, Ref: refDir, Size: int64(len("dir")), SHA256: sha256Hex([]byte("dir")),
					}},
				},
			}
			oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{Assets: assets})
			if err == nil || !strings.Contains(err.Error(), "input_files") || !strings.Contains(err.Error(), "collide") {
				t.Fatalf("Run err = %v, want input_files collision", err)
			}
			if oc != engine.OutcomePermanentFailure {
				t.Fatalf("Outcome = %q, want permanent_failure", oc)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("Exec calls = %+v, want none before collision", fake.Calls)
			}
		})
	}
}

func TestRunCodeStepOutputFilesPathTemplated(t *testing.T) {
	// output_files paths are template-substituted (like run: and idempotency_key),
	// so a bare-list path "/out/{{ input.cve }}.json" captures + commits keyed by
	// the SUBSTITUTED path. Regression: the cve-feasibility assemble step used
	// "/work/records/{{ input.cve_id }}.partial.json" and AWF looked for the
	// literal, un-substituted filename → CaptureFiles "could not find the file".
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disp := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"lab": h}}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", map[string]any{"cve": "CVE-2025-29927"})

	// The script writes the SUBSTITUTED path; seed it so the post-exec capture finds it.
	fake.ProgramExec("./assemble.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(h, "/out/CVE-2025-29927.json", []byte(`{"ok":1}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "assemble", Container: "lab", Run: "./assemble.sh",
				OutputFiles: ir.OutputFiles{{Path: "/out/{{ input.cve }}.json"}},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.Completed["assemble"]
	if !ok {
		t.Fatal("RunState.Completed missing 'assemble'")
	}
	if _, ok := nr.Files["/out/CVE-2025-29927.json"]; !ok {
		t.Errorf("nr.Files = %v, want key /out/CVE-2025-29927.json (output_files path must be template-substituted)", nr.Files)
	}
	if _, bad := nr.Files["/out/{{ input.cve }}.json"]; bad {
		t.Error("nr.Files contains the LITERAL un-substituted path; output_files path was not templated")
	}
}

func TestRunOutputFileContractSchemaRefUsesRunStartedAsset(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExecWithFiles("./produce.sh", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/summary.json": []byte(`{"status":42}`)})
	schemaBytes := []byte(`{"type":"object","required":["status"],"properties":{"status":{"type":"string"}}}`)
	ref, err := blobs.Put(schemaBytes)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		ID: "artifact-contract", Version: 1,
		Assets:     map[string]string{"summary_schema": "schemas/summary.json"},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "produce", Container: "lab", Run: "./produce.sh",
				Retry: &ir.RetryPolicy{Attempts: 1},
				OutputFiles: ir.OutputFiles{{
					Name:      "summary",
					Path:      "/out/summary.json",
					Format:    "json",
					SchemaRef: "asset.summary_schema",
				}},
			},
		},
	}
	asset := engine.RunStartedAsset{
		DeclaredPath: "schemas/summary.json",
		Files: []engine.RunStartedAssetFile{{
			Path: ".", Ref: ref, Size: int64(len(schemaBytes)), SHA256: sha256Hex(schemaBytes),
		}},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{
		Assets: map[string]engine.RunStartedAsset{"summary_schema": asset},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact contract") || !strings.Contains(err.Error(), "schema validation") {
		t.Fatalf("Run err = %v, want artifact contract schema validation failure", err)
	}
	if oc != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want retryable_failure", oc)
	}
	if _, ok := rs.Completed["produce"]; ok {
		t.Fatal("invalid artifact contract committed node.completed")
	}
}

func TestRunOutputFilesDuplicateSubstitutedPathRejectedBeforeExec(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"name": "summary"}
	fake.ProgramExecWithFiles("./produce.sh", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/summary.json": []byte(`{"id":"bad"}`)})
	wf := &ir.Workflow{
		ID: "artifact-contract-duplicate", Version: 1,
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "produce", Container: "lab", Run: "./produce.sh",
				OutputFiles: ir.OutputFiles{
					{
						Name:   "strict",
						Path:   "/out/{{ input.name }}.json",
						Format: "json",
						Schema: &ir.JSONSchema{
							"type":       "object",
							"required":   []any{"id"},
							"properties": map[string]any{"id": map[string]any{"type": "integer"}},
						},
					},
					{Name: "alias", Path: "/out/summary.json", Format: "json"},
				},
			},
		},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate output_files path") {
		t.Fatalf("Run err = %v, want duplicate output_files path failure", err)
	}
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want permanent_failure", oc)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("Exec calls = %+v, want none before ambiguous artifact contracts dispatch", fake.Calls)
	}
	if _, ok := rs.Completed["produce"]; ok {
		t.Fatal("duplicate output_files path committed node.completed")
	}
}

func TestRunOutputFileBadContractDefinitionFailsBeforeExec(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./must-not-run.sh", container.ExecResult{ExitCode: 0}, nil)
	wf := &ir.Workflow{
		ID: "artifact-contract", Version: 1,
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "produce", Container: "lab", Run: "./must-not-run.sh",
				OutputFiles: ir.OutputFiles{{
					Name:   "summary",
					Path:   "/out/summary.txt",
					Format: "text",
				}},
			},
		},
	}
	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "format must be json or jsonl") {
		t.Fatalf("Run err = %v, want bad contract definition failure", err)
	}
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want permanent_failure", oc)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("Exec calls = %+v, want none before bad contract definition", fake.Calls)
	}
	if _, ok := rs.Completed["produce"]; ok {
		t.Fatal("bad artifact contract committed node.completed")
	}
}

func TestRunNamedOutputFileRefWithTemplatedPath(t *testing.T) {
	// A NAMED output_files artifact whose path is templated must still resolve when
	// referenced by a later step's input_files: the capture key (commit.go) and the
	// ref lookup (resolveInputFiles → ResolveArtifactPath) both substitute the path,
	// so they agree. Without templating the ref-lookup side too, the consumer would
	// look up the un-substituted declared path and miss the committed artifact.
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	labH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	boxH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}
	disp := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"lab": labH, "box": boxH}}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", map[string]any{"cve": "CVE-2025-29927"})

	sentinel := []byte("report body\n")
	fake.ProgramExec("./recon.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(labH, "/out/CVE-2025-29927.md", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake.ProgramExec("./hunt.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}, "box": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "recon", Container: "lab", Run: "./recon.sh",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/{{ input.cve }}.md"}},
			},
			&ir.CodeStep{
				ID: "hunt", Container: "box", Run: "./hunt.sh",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	got, err := fake.CaptureFiles(context.Background(), boxH, []string{"/work/report.md"})
	if err != nil {
		t.Fatalf("CaptureFiles box: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(sentinel) {
		t.Errorf("staged into box = %+v, want one file %q (named templated-path artifact must resolve)", got, sentinel)
	}
}

func TestRunInputFilesDestinationPathTemplated(t *testing.T) {
	// input_files destination paths are template-substituted against the consumer
	// scope before staging, matching output_files path templating. This is needed
	// for dynamic record paths such as /work/records/{{ input.cve_id }}.json.
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	labH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	boxH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}
	disp := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"lab": labH, "box": boxH}}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", map[string]any{"cve": "CVE-2025-29927"})

	sentinel := []byte(`{"ok":true}`)
	fake.ProgramExec("./make.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(labH, "/out/record.json", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake.ProgramExec("./consume.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}, "box": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "make", Container: "lab", Run: "./make.sh",
				OutputFiles: ir.OutputFiles{{Name: "record", Path: "/out/record.json"}},
			},
			&ir.CodeStep{
				ID: "consume", Container: "box", Run: "./consume.sh",
				InputFiles: map[string]string{"/work/records/{{ input.cve }}.json": "step.make.files.record"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	got, err := fake.CaptureFiles(context.Background(), boxH, []string{"/work/records/CVE-2025-29927.json"})
	if err != nil {
		t.Fatalf("CaptureFiles templated destination: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(sentinel) {
		t.Errorf("staged into templated destination = %+v, want %q", got, sentinel)
	}
}

func TestRunCodeStepFailureHaltsSubsequentSteps(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step1.sh", container.ExecResult{ExitCode: 78}, nil)
	fake.ProgramExec("./step2.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "step1", Container: "lab", Run: "./step1.sh"},
		&ir.CodeStep{ID: "step2", Container: "lab", Run: "./step2.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	_, _ = engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if len(fake.Calls) != 1 {
		t.Errorf("fake.Calls len = %d, want 1 (step2 must NOT dispatch after step1 fails)", len(fake.Calls))
	}
	if fake.Calls[0].Run != "./step1.sh" {
		t.Errorf("fake.Calls[0].Run = %q, want %q", fake.Calls[0].Run, "./step1.sh")
	}
}

func TestRunCodeStepTemplateErrorIsPermanent(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bad", Container: "lab", Run: "./run.sh {{ step.nonexistent.field }}"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure (template error is author bug, slice 2.5 DQ7)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want AWF4002")
	}
	if !strings.Contains(err.Error(), "AWF4002") {
		t.Errorf("err = %v, want mention of AWF4002", err)
	}
	events, _ := log.Fold()
	var found bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "bad" {
			found = true
		}
	}
	if !found {
		t.Errorf("no node.failed event; events: %+v", events)
	}
}

func TestRunCodeStepLiveTapWritesStepIDPrefixedChunks(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./chunky.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("done\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("line 1\n")},
		{Stream: "stdout", Data: []byte("line 2\n")},
		{Stream: "stderr", Data: []byte("warn: thing happened\n")},
	})

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "chunky", Container: "lab", Run: "./chunky.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	var tap bytes.Buffer
	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{Tap: &tap}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := tap.String()
	wantLines := []string{
		"[chunky] line 1\n",
		"[chunky] line 2\n",
		"[chunky] warn: thing happened\n",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("tap missing %q; got %q", w, out)
		}
	}
}

func TestRunSkipsAlreadyCompletedNodes(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	exit := 0
	rs.Completed["already_done"] = engine.NodeResult{
		Outcome:  engine.OutcomeOK,
		ExitCode: &exit,
		Stdout:   []byte("previously committed\n"),
	}
	fake.ProgramExec("./would-fail.sh", container.ExecResult{ExitCode: 1}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "already_done", Container: "lab", Run: "./would-fail.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (committed steps must NOT re-execute)", len(fake.Calls))
	}
}

// TestRunPhase2UnsupportedKindsAllErrorWithSentinel was here until slice 5.2 task 9:
// agent was the last remaining case; it now routes to runAgentStep (no longer notImpl).
// Signal/parallel/gate/map were each removed from this table in their respective slices.

func TestRunCodeStepDefaultAttemptsOnce(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./fails.sh", container.ExecResult{ExitCode: 1}, nil)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "fails", Container: "lab", Run: "./fails.sh"},
	}}

	oc, err := engine.Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomeRetryableFailure || err == nil {
		t.Fatalf("Run = (%q, %v), want retryable failure", oc, err)
	}
	if got := len(fake.Calls); got != 1 {
		t.Errorf("dispatches = %d, want 1 without an explicit retry block", got)
	}
	events, _ := log.Fold()
	for _, ev := range events {
		if ev.Type == engine.EventRetryAttempt {
			t.Errorf("unexpected retry.attempt event: %+v", ev)
		}
	}
}

func TestRunCodeStepRetryableExhaustionAppendsNodeFailed(t *testing.T) {
	// After retry exhaustion, the interpreter's runCodeStep MUST route through
	// failStep with outcome=retryable_failure — the node.failed event records
	// the LAST attempt's error (matches RunWithRetry's "exhausted-as-failure"
	// contract). Distinct from the permanent_failure path (exit code in
	// NonRetryableExitCodes), which is already covered by
	// TestRunCodeStepFailureAppendsNodeFailed.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	// Exit 1 is a generic nonzero (retryable). The explicit two-attempt policy
	// exhausts and RunWithRetry returns the last attempt's error.
	fake.ProgramExec("./flaky.sh", container.ExecResult{
		ExitCode: 1,
		Stdout:   []byte("transient failure\n"),
	}, nil)

	// Override the retry policy so attempts run instantly (no real sleeps);
	// the fake clock advances synthetically via clock.Fake.Sleep, but we
	// still pay the overhead of the 3-attempt loop. Use a CodeStep with an
	// explicit no-backoff override so the test is fast.
	noBackoff := "none"
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID: "flaky", Container: "lab", Run: "./flaky.sh",
			Retry: &ir.RetryPolicy{Attempts: 2, Backoff: noBackoff},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable_failure", oc)
	}
	if err == nil {
		t.Fatal("err = nil; want last-attempt error")
	}
	// No node.completed for the failed step.
	if _, ok := rs.Completed["flaky"]; ok {
		t.Error("RunState.Completed has 'flaky' — failed steps must NOT commit")
	}
	// node.failed event landed with outcome=retryable_failure.
	events, _ := log.Fold()
	var failedFound bool
	var failedOutcome string
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "flaky" {
			failedFound = true
			var d engine.NodeFailedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal node.failed: %v", err)
			}
			failedOutcome = d.Outcome
		}
	}
	if !failedFound {
		t.Errorf("no node.failed event for 'flaky'; events: %+v", events)
	}
	if failedOutcome != string(engine.OutcomeRetryableFailure) {
		t.Errorf("node.failed outcome = %q, want %q", failedOutcome, engine.OutcomeRetryableFailure)
	}
	// Verify all retry attempts actually ran (2 dispatches for Attempts:2).
	if len(fake.Calls) != 2 {
		t.Errorf("fake.Calls len = %d, want 2 (retry exhaustion)", len(fake.Calls))
	}
}

func TestRunUnknownContainerIsInternalError(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bad", Container: "no_such_container", Run: "./whatever.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != "" {
		t.Errorf("Outcome = %q, want empty (internal error, not a step outcome)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want unknown-container error")
	}
}

func TestRunIfThenBranchTaken(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_then.sh", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./step_in_else.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Input = map[string]any{"do_it": true}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fake.Calls))
	}
	if fake.Calls[0].Run != "./step_in_then.sh" {
		t.Errorf("ran %q, want ./step_in_then.sh", fake.Calls[0].Run)
	}
	events, _ := log.Fold()
	var bt *engine.BranchTakenData
	var btPath string
	for _, e := range events {
		if e.Type == engine.EventBranchTaken {
			var d engine.BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal branch.taken: %v", err)
			}
			bt = &d
			btPath = e.Path
		}
	}
	if bt == nil {
		t.Fatal("no branch.taken event in log")
	}
	if bt.Which != "then" || btPath != "if[0]" {
		t.Errorf("branch.taken = %+v at path %q, want {Which:then} at if[0]", bt, btPath)
	}
	if rs.Branches["if[0]"] != "then" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "then")
	}
}

func TestRunIfElseBranchTaken(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_else.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	_, _ = engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "./step_in_else.sh" {
		t.Errorf("dispatched %+v, want only ./step_in_else.sh", fake.Calls)
	}
	if rs.Branches["if[0]"] != "else" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "else")
	}
}

func TestRunIfNoElseFalseCondIsNoOp(t *testing.T) {
	// Spec §5.1: "A false cond with no else is a no-op." Branch.taken still
	// fires (Which:"else") so resume knows the decision was made.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./never.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("dispatched %d cmds, want 0", len(fake.Calls))
	}
	if rs.Branches["if[0]"] != "else" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "else")
	}
	// Verify the branch.taken event landed in the log (not just in the in-mem
	// map). A divergence between rs.Branches and the log would mean resume
	// re-evaluates the cond — the test was previously weaker than the spec.
	events, _ := log.Fold()
	var bt *engine.BranchTakenData
	for _, e := range events {
		if e.Type == engine.EventBranchTaken && e.Path == "if[0]" {
			var d engine.BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal branch.taken: %v", err)
			}
			bt = &d
		}
	}
	if bt == nil {
		t.Fatal("no branch.taken event in log")
	}
	if bt.Which != "else" {
		t.Errorf("branch.taken Which = %q, want %q", bt.Which, "else")
	}
}

func TestRunIfResumeSkipsCondEvaluation(t *testing.T) {
	// rs.Branches[if[0]]="then" simulates a resume where the branch decision
	// was already committed. Re-evaluating cond would be wrong — cond depends
	// on inputs/step outputs that may have changed.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_then.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Branches["if[0]"] = "then"
	// Input would evaluate to else if re-evaluated.
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "./step_in_then.sh" {
		t.Errorf("ran %+v, want ./step_in_then.sh (recorded branch)", fake.Calls)
	}
	events, _ := log.Fold()
	var branchTakenCount int
	for _, e := range events {
		if e.Type == engine.EventBranchTaken {
			branchTakenCount++
		}
	}
	if branchTakenCount != 0 {
		t.Errorf("emitted %d branch.taken events on resume, want 0 (recorded branch)", branchTakenCount)
	}
}

func TestRunIfCondTypeMismatchIsPermanent(t *testing.T) {
	// Spec §7: bounded evaluator, no coercion. Non-bool top-level cond is
	// AWF4003. Per DQ7, route as permanent_failure for the if NODE.
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"count": 5} // int, not bool

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.count }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./never.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4003") {
		t.Errorf("err = %v, want AWF4003", err)
	}
	events, _ := log.Fold()
	var found bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "if[0]" {
			found = true
		}
	}
	if !found {
		t.Errorf("no node.failed event for if[0]; events: %+v", events)
	}
}

func TestRunLoopWithMaxItersOnly(t *testing.T) {
	// 3-iter loop, no until. Each iter runs the body once; loop.iter fires
	// once per completed iter; LoopIters[path] ends at 3.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	max := 3
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if rs.LoopIters["loop[0]"] != 3 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 3", rs.LoopIters["loop[0]"])
	}
	if len(fake.Calls) != 3 {
		t.Errorf("body dispatched %d times, want 3", len(fake.Calls))
	}
	events, _ := log.Fold()
	var iterEvents int
	for _, e := range events {
		if e.Type == engine.EventLoopIter && e.Path == "loop[0]" {
			iterEvents++
		}
	}
	if iterEvents != 3 {
		t.Errorf("emitted %d loop.iter events, want 3", iterEvents)
	}
}

func TestRunLoopUntilExitsBeforeMaxIters(t *testing.T) {
	// Body returns a typed output `done: true`; until reads it → true on
	// iter 1 → loop exits after iter 1, well before MaxIters=5. Verifies
	// the until-true path takes precedence over MaxIters.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"done":true}`),
	}, nil)
	max := 5
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			Until:    refExpr("{{ step.body_step.done }}"),
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{
					ID: "body_step", Container: "lab", Run: "./body.sh",
					OutputSchema: &ir.JSONSchema{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"done"},
						"properties":           map[string]any{"done": map[string]any{"type": "boolean"}},
					},
				},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if rs.LoopIters["loop[0]"] != 1 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 1 (until true on iter 1)", rs.LoopIters["loop[0]"])
	}
	if len(fake.Calls) != 1 {
		t.Errorf("body dispatched %d times, want 1", len(fake.Calls))
	}
}

func TestRunLoopUntilEvalErrorIsPermanent(t *testing.T) {
	// Mirror of TestRunIfCondTypeMismatchIsPermanent for the loop's until.
	// Body returns a non-bool typed output (`count: 5`); until's top-level
	// eval result is an integer, not a bool → AWF4003.
	//
	// Subtle invariant exercised here: the until eval happens AFTER the body
	// completes, so the loop.iter event for iter 1 WAS emitted before the
	// until error fires. node.failed is appended at the loop's path (not
	// the body's), per runLoop's failStep call.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"count":5}`),
	}, nil)
	max := 3
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			Until:    refExpr("{{ step.body_step.count }}"), // int, not bool
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{
					ID: "body_step", Container: "lab", Run: "./body.sh",
					OutputSchema: &ir.JSONSchema{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"count"},
						"properties":           map[string]any{"count": map[string]any{"type": "integer"}},
					},
				},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4003") {
		t.Errorf("err = %v, want AWF4003", err)
	}
	// The body's iter 1 DID complete — loop.iter was emitted before the
	// until error fired.
	events, _ := log.Fold()
	var loopIterCount int
	var nodeFailedPath string
	for _, e := range events {
		switch e.Type {
		case engine.EventLoopIter:
			if e.Path == "loop[0]" {
				loopIterCount++
			}
		case engine.EventNodeFailed:
			nodeFailedPath = e.Path
		}
	}
	if loopIterCount != 1 {
		t.Errorf("loop.iter events = %d, want 1 (body completed before until error)", loopIterCount)
	}
	if nodeFailedPath != "loop[0]" {
		t.Errorf("node.failed at path %q, want %q (loop's path, not body's)", nodeFailedPath, "loop[0]")
	}
	// rs.LoopIters[loop[0]] = 1 — iter 1 DID complete, but the loop terminated
	// in failure after that iter's until evaluation.
	if rs.LoopIters["loop[0]"] != 1 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 1 (iter 1 committed before until error)", rs.LoopIters["loop[0]"])
	}
}

func TestRunLoopBodyFailureDoesNotEmitLoopIter(t *testing.T) {
	// DQ8: if body[K] fails, loop.iter{K} is NOT emitted.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./flaky.sh", container.ExecResult{ExitCode: 78}, nil)
	max := 3
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "flaky", Container: "lab", Run: "./flaky.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, _ := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if rs.LoopIters["loop[0]"] != 0 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 0 (iter 1 failed mid-flight, never committed)", rs.LoopIters["loop[0]"])
	}
	events, _ := log.Fold()
	var iterEvents int
	for _, e := range events {
		if e.Type == engine.EventLoopIter {
			iterEvents++
		}
	}
	if iterEvents != 0 {
		t.Errorf("emitted %d loop.iter events, want 0 (body failed before iter completed)", iterEvents)
	}
}

func TestRunLoopResumeContinuesFromLastCompletedIter(t *testing.T) {
	// Pre-populate rs.LoopIters[loop[0]] = 2 — simulates resume where iters
	// 1 and 2 committed and iter 3 was in-flight.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.LoopIters["loop[0]"] = 2

	max := 4
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.Calls) != 2 {
		t.Errorf("body dispatched %d times, want 2 (resume from iter 3 to max=4)", len(fake.Calls))
	}
	if rs.LoopIters["loop[0]"] != 4 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 4", rs.LoopIters["loop[0]"])
	}
}

func TestRunLoopBodyStepPathIncludesIterSuffix(t *testing.T) {
	// node.completed events must use path "loop[0].body.iter-K.body_step" —
	// the addressing grammar from slice 2.1.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	max := 2
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, _ := log.Fold()
	wantPaths := map[string]bool{
		"loop[0].body.iter-1.body_step": false,
		"loop[0].body.iter-2.body_step": false,
	}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			if _, want := wantPaths[e.Path]; want {
				wantPaths[e.Path] = true
			}
		}
	}
	for path, found := range wantPaths {
		if !found {
			t.Errorf("no node.completed event at path %q", path)
		}
	}
}

func TestRunLoopNeitherUntilNorMaxIsInternalError(t *testing.T) {
	// Slice 2.5 R7: validator (ir/validate_structural.go:86) enforces
	// "at least one of until / max_iters" (AWF §5.2). If validation
	// regresses, the runtime must fail LOUD — silently exiting on iter 1
	// would mask the bug.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			// Until nil, MaxIters nil — would be rejected by validator,
			// but the runtime defends.
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if oc != "" {
		t.Errorf("Outcome = %q, want empty (internal error)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want validator-regression error")
	}
	if !strings.Contains(err.Error(), "validator regression") {
		t.Errorf("err = %v, want mention of 'validator regression'", err)
	}
}
