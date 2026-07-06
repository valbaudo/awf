//go:build integ

package docker

import (
	"context"
	"testing"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	cont "github.com/valbaudo/awf/container"
)

// testWorkdir is a non-root WORKDIR distinct from both alpine's image
// default ("" / "/") and the tar-extract root ("/") that CopyFromContainer
// would otherwise resolve a relative path against — chosen so the test can
// only pass if CaptureFiles actually joins against the container's
// Config.WorkingDir rather than "/" or "" by coincidence.
const testWorkdir = "/custom/work"

// newWorkdirContainer creates an alpine container directly via the Docker
// SDK (bypassing Backend.Create, same rationale as exec_integ_test.go's
// newAlpineContainer: inject `sleep infinity` so Exec has a live target) with
// Config.WorkingDir explicitly set. The docker daemon treats an
// SDK-supplied WorkingDir identically to one baked into the image's
// Dockerfile for exec-cwd purposes (exec.go's ContainerExecCreate sets no
// WorkingDir override, so the exec inherits whichever Config.WorkingDir the
// container was created with) — so this exercises the exact F8 code path
// (CaptureFiles inspecting Config.WorkingDir at capture time) without
// depending on any third-party image's undocumented WORKDIR.
func newWorkdirContainer(t *testing.T, cli *client.Client, b *Backend, workdir string) cont.Handle {
	t.Helper()
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}
	name := containerName(b.runID, "workdir-lab")
	resp, err := cli.ContainerCreate(ctx,
		&dockerContainer.Config{Image: alpineDigest, Cmd: []string{"sleep", "infinity"}, WorkingDir: workdir},
		&dockerContainer.HostConfig{},
		nil, nil, name,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := cli.ContainerStart(ctx, resp.ID, dockerContainer.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	b.mu.Lock()
	b.handles[resp.ID] = registeredContainer{kind: kindImage, dockerID: resp.ID}
	b.mu.Unlock()
	h := cont.Handle{Name: "workdir-lab", ID: resp.ID}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	return h
}

// TestDockerCaptureFilesRelativeAtWorkdir is the F8 end-to-end proof: a run:
// step writes a relative-path file into the container's WORKDIR, and
// output_files.path declares it relative — CaptureFiles must resolve it
// against Config.WorkingDir (not "/") to find it. Requires a live Docker
// daemon; runs under -tags integ on a docker host / cve-runner, not on the
// controller box (no docker there).
func TestDockerCaptureFilesRelativeAtWorkdir(t *testing.T) {
	cli, b := newTestBackend(t, "capture-workdir")
	h := newWorkdirContainer(t, cli, b, testWorkdir)
	ctx := context.Background()

	// Sanity-check the fixture itself actually has the non-default WORKDIR
	// this test depends on, before trusting the CaptureFiles assertion below.
	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if info.Config == nil || info.Config.WorkingDir != testWorkdir {
		t.Fatalf("fixture WorkingDir = %+v; want %q", info.Config, testWorkdir)
	}

	chunks, result, err := b.Exec(ctx, h, cont.Cmd{Run: "mkdir -p sub && echo hello > sub/out.txt"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range chunks {
	}
	r := <-result
	if r.Err != nil {
		t.Fatalf("Exec result.Err: %v", r.Err)
	}
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode = %d; want 0", r.ExitCode)
	}

	// The declared output_files.path is relative, exactly as an author would
	// write it in the workflow — this is the path F8 makes resolvable.
	captured, err := b.CaptureFiles(ctx, h, []string{"sub/out.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles(relative %q): %v", "sub/out.txt", err)
	}
	if len(captured) != 1 {
		t.Fatalf("len(captured) = %d; want 1", len(captured))
	}
	if captured[0].Path != "sub/out.txt" {
		t.Errorf("captured[0].Path = %q; want the original requested path %q", captured[0].Path, "sub/out.txt")
	}
	if got := string(captured[0].Content); got != "hello\n" {
		t.Errorf("captured[0].Content = %q; want %q", got, "hello\n")
	}
}
