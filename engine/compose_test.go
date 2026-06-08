package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func pinnedComposeBytes(service string) []byte {
	return []byte("services:\n  " + service + ":\n    image: example.com/" + service + "@sha256:" + strings.Repeat("0", 64) + "\n")
}

func runtimeComposeWorkflow(body ir.NodeList) (*ir.Workflow, *ir.Compose) {
	node := &ir.Compose{
		As:      "lab",
		From:    "step.lab_gen.files.compose",
		Service: "{{ step.lab_gen.service }}",
		Body:    body,
	}
	return &ir.Workflow{
		ID: "runtime-compose", Version: 1,
		Containers: map[string]ir.Container{
			"runner": {Image: "oci://example.com/runner@sha256:" + strings.Repeat("0", 64)},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:        "lab_gen",
				Container: "runner",
				Run:       "./generate-lab.sh",
				OutputSchema: &ir.JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"service"},
					"properties": map[string]any{
						"service": map[string]any{"type": "string"},
					},
				},
				OutputFiles: ir.OutputFiles{{Name: "compose", Path: "/work/lab/compose.yml"}},
			},
			node,
		},
	}, node
}

type composeRig struct {
	fake  *container.Fake
	ld    *LocalDispatcher
	log   *state.InMemoryLog
	blobs *state.InMemoryBlobs
	clk   *clock.Fake
	rs    *RunState
}

func newComposeRig(t *testing.T, wf *ir.Workflow, composeBytes []byte, service string) *composeRig {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	blobs := state.NewInMemoryBlobs()
	ref, err := blobs.Put(composeBytes)
	if err != nil {
		t.Fatalf("put compose blob: %v", err)
	}
	rs := NewRunState(testRunID, testDigest, nil)
	rs.RecordCompleted("lab_gen", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"service": service},
		Files:   map[string]string{"/work/lab/compose.yml": ref},
	})
	return &composeRig{
		fake:  fake,
		ld:    &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{}},
		log:   state.NewInMemoryLog(clk),
		blobs: blobs,
		clk:   clk,
		rs:    rs,
	}
}

func TestRunComposePromotesGeneratedArtifactAndRoutesBody(t *testing.T) {
	wf, node := runtimeComposeWorkflow(ir.NodeList{
		&ir.CodeStep{ID: "smoke", Container: "lab", Run: "./smoke.sh"},
	})
	rig := newComposeRig(t, wf, pinnedComposeBytes("web"), "web")
	rig.fake.ProgramExec("./smoke.sh", container.ExecResult{ExitCode: 0, Stdout: []byte("ok\n")}, nil)

	oc, err := runCompose(context.Background(), node, "compose[1]", wf, rig.rs, rig.ld, rig.log, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runCompose = (%q, %v), want (ok, nil)", oc, err)
	}
	if _, ok := rig.rs.LookupCompleted("compose[1].body.smoke"); !ok {
		t.Fatal("body step was not committed at compose[1].body.smoke")
	}
	if len(rig.fake.CreateSpecs) != 1 {
		t.Fatalf("CreateSpecs len = %d, want 1", len(rig.fake.CreateSpecs))
	}
	spec := rig.fake.CreateSpecs[0]
	if spec.Service != "web" {
		t.Errorf("created service = %q, want web", spec.Service)
	}
	if !bytes.Equal(spec.Compose, pinnedComposeBytes("web")) {
		t.Errorf("created compose bytes differ from committed artifact")
	}
	if !strings.Contains(spec.Name, "compose-1") || !strings.Contains(spec.Name, "lab") {
		t.Errorf("runtime compose spec.Name = %q, want path-derived name containing compose-1 and lab", spec.Name)
	}
	if len(rig.fake.DestroyCalls) != 1 {
		t.Fatalf("DestroyCalls len = %d, want 1", len(rig.fake.DestroyCalls))
	}
}

func TestRunComposeServiceOverrideReachesScopedHandle(t *testing.T) {
	wf, node := runtimeComposeWorkflow(ir.NodeList{
		&ir.CodeStep{ID: "api", Container: "lab:api", Run: "./api.sh"},
	})
	composeBytes := []byte("services:\n  web:\n    image: example.com/web@sha256:" + strings.Repeat("0", 64) + "\n  api:\n    image: example.com/api@sha256:" + strings.Repeat("1", 64) + "\n")
	rig := newComposeRig(t, wf, composeBytes, "web")
	rig.fake.ProgramExec("./api.sh", container.ExecResult{ExitCode: 0}, nil)

	oc, err := runCompose(context.Background(), node, "compose[1]", wf, rig.rs, rig.ld, rig.log, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runCompose = (%q, %v), want (ok, nil)", oc, err)
	}
	if len(rig.fake.ExecHandles) != 1 {
		t.Fatalf("ExecHandles len = %d, want 1", len(rig.fake.ExecHandles))
	}
	if got := rig.fake.ExecHandles[0].Service; got != "api" {
		t.Errorf("body exec handle service = %q, want api override", got)
	}
}

func TestRunComposeDestroyFailureFailsWhenBodySucceeded(t *testing.T) {
	wf, node := runtimeComposeWorkflow(ir.NodeList{
		&ir.CodeStep{ID: "smoke", Container: "lab", Run: "./smoke.sh"},
	})
	rig := newComposeRig(t, wf, pinnedComposeBytes("web"), "web")
	rig.fake.ProgramExec("./smoke.sh", container.ExecResult{ExitCode: 0}, nil)
	destroyErr := errors.New("destroy failed")
	rig.ld.Backend = &destroyFailingBackend{inner: rig.fake, err: destroyErr}

	oc, err := runCompose(context.Background(), node, "compose[1]", wf, rig.rs, rig.ld, rig.log, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("runCompose outcome = %q, want retryable_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "runtime compose destroy") {
		t.Fatalf("runCompose err = %v, want runtime compose destroy error", err)
	}
	if !errors.Is(err, destroyErr) {
		t.Fatalf("runCompose err = %v, want to wrap destroy error", err)
	}
}

func TestRunComposeInvalidComposeFailsBeforeCreate(t *testing.T) {
	wf, node := runtimeComposeWorkflow(ir.NodeList{
		&ir.CodeStep{ID: "smoke", Container: "lab", Run: "./smoke.sh"},
	})
	rig := newComposeRig(t, wf, []byte("not valid yaml\n  - oops\n"), "web")

	oc, err := runCompose(context.Background(), node, "compose[1]", wf, rig.rs, rig.ld, rig.log, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomePermanentFailure {
		t.Fatalf("runCompose outcome = %q, want permanent_failure", oc)
	}
	if err == nil {
		t.Fatal("runCompose err = nil, want validation error")
	}
	if len(rig.fake.CreateSpecs) != 0 {
		t.Fatalf("CreateSpecs len = %d, want 0 (invalid bytes must fail before Create)", len(rig.fake.CreateSpecs))
	}
}

type destroyFailingBackend struct {
	inner *container.Fake
	err   error
}

func (b *destroyFailingBackend) Capabilities() container.Caps {
	return b.inner.Capabilities()
}

func (b *destroyFailingBackend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	return b.inner.Create(ctx, spec)
}

func (b *destroyFailingBackend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	return b.inner.Exec(ctx, h, cmd)
}

func (b *destroyFailingBackend) CaptureFiles(ctx context.Context, h container.Handle, paths []string) ([]container.CapturedFile, error) {
	return b.inner.CaptureFiles(ctx, h, paths)
}

func (b *destroyFailingBackend) CopyTo(ctx context.Context, h container.Handle, files []container.InputFile) error {
	return b.inner.CopyTo(ctx, h, files)
}

func (b *destroyFailingBackend) Snapshot(ctx context.Context, h container.Handle) (container.SnapshotRef, error) {
	return b.inner.Snapshot(ctx, h)
}

func (b *destroyFailingBackend) Restore(ctx context.Context, ref container.SnapshotRef, name string) (container.Handle, error) {
	return b.inner.Restore(ctx, ref, name)
}

func (b *destroyFailingBackend) Destroy(ctx context.Context, h container.Handle) error {
	if err := b.inner.Destroy(ctx, h); err != nil {
		return fmt.Errorf("inner destroy: %w", err)
	}
	return b.err
}

func TestRuntimeComposeFailureIsCatchable(t *testing.T) {
	wf, node := runtimeComposeWorkflow(ir.NodeList{
		&ir.CodeStep{ID: "smoke", Container: "lab", Run: "./smoke.sh"},
	})
	wf.Graph[1] = &ir.Try{
		Do: ir.NodeList{node},
		Catch: ir.NodeList{
			&ir.CodeStep{ID: "cannot_build_lab", Container: "runner", Run: "./emit-skip.sh cannot_build_lab"},
		},
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	runner, err := fake.Create(context.Background(), container.ContainerSpec{Name: "runner"})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	fake.ProgramExecWithFiles("./generate-lab.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"service":"web"}`),
	}, nil, map[string][]byte{
		"/work/lab/compose.yml": []byte("not valid yaml\n  - oops\n"),
	})
	fake.ProgramExec("./emit-skip.sh cannot_build_lab", container.ExecResult{ExitCode: 0}, nil)

	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	rs := NewRunState(testRunID, testDigest, nil)
	disp := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"runner": runner}}

	oc, err := Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, log, blobs, clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("Run = (%q, %v), want (ok, nil)", oc, err)
	}
	if _, ok := rs.LookupCompleted("try[1].catch.cannot_build_lab"); !ok {
		t.Fatal("catch step cannot_build_lab was not committed")
	}
	// Only the pre-created runner handle should exist; invalid runtime compose
	// bytes must not reach Backend.Create.
	if len(fake.CreateSpecs) != 1 {
		t.Fatalf("CreateSpecs len = %d, want only the runner create", len(fake.CreateSpecs))
	}
}
