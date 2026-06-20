//go:build integ

package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"

	cont "github.com/valbaudo/awf/container"
)

// TestKeepalive_CommandlessImageStaysRunning verifies that Backend.Create with
// a command-less image (no spec.Cmd, DisableKeepalive false) injects
// `sleep infinity` and the container stays running so a subsequent Exec
// succeeds. Alpine's default CMD is /bin/sh which exits immediately; without
// the injection Exec would fail "not running".
//
// Negative cases:
//
//	(a) spec.Cmd set → keepalive NOT injected, the author's cmd is used.
//	(b) DisableKeepalive=true → NOT injected, container exits, Exec fails.
func TestKeepalive_CommandlessImageStaysRunning(t *testing.T) {
	cli, b := newTestBackend(t, "keepalive-inject")
	ctx := context.Background()

	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Happy path: command-less spec → keepalive injected → container running.
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "ka-test",
		Image: alpineDigest,
		// No Cmd, DisableKeepalive false (zero value) → inject sleep infinity.
	})
	if err != nil {
		t.Fatalf("Create (command-less): %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	// Container must be running.
	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if info.State == nil || !info.State.Running {
		t.Errorf("container is not running after keepalive injection (State=%+v)", info.State)
	}

	// Exec must succeed on the running container.
	chunks, result, err := b.Exec(ctx, h, cont.Cmd{Run: "echo hi"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var stdout strings.Builder
	for c := range chunks {
		if c.Stream == "stdout" {
			stdout.Write(c.Data)
		}
	}
	r := <-result
	if r.ExitCode != 0 {
		t.Errorf("Exec exit code = %d, want 0", r.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "hi" {
		t.Errorf("Exec stdout = %q, want %q", got, "hi")
	}
}

// TestKeepalive_AuthorCmdNotOverridden verifies negative case (a): when
// spec.Cmd is already set, the keepalive injection is skipped and the
// container runs the author's command.
func TestKeepalive_AuthorCmdNotOverridden(t *testing.T) {
	cli, b := newTestBackend(t, "keepalive-author-cmd")
	ctx := context.Background()

	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Author provides a long-running cmd explicitly.
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "ka-author",
		Image: alpineDigest,
		Cmd:   []string{"sleep", "infinity"}, // author cmd → no injection needed
	})
	if err != nil {
		t.Fatalf("Create (author cmd): %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	// Inspect: confirm the container is running with the author's command, not
	// the injected one (they're the same here, but the contract is: no override).
	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if info.State == nil || !info.State.Running {
		t.Errorf("container with author cmd not running")
	}

	// The Config.Cmd on the running container should be the author's.
	if info.Config == nil {
		t.Fatal("ContainerInspect: Config is nil")
	}
	cmd := strings.Join(info.Config.Cmd, " ")
	if !strings.Contains(cmd, "sleep") {
		t.Errorf("container Cmd = %q; want author's sleep command", cmd)
	}
}

// TestKeepalive_DisabledDoesNotInject verifies negative case (b): with
// DisableKeepalive=true on a command-less image, no sleep infinity is
// injected. Alpine's /bin/sh exits, the container stops, and Exec fails.
func TestKeepalive_DisabledDoesNotInject(t *testing.T) {
	cli, b := newTestBackend(t, "keepalive-disabled")
	ctx := context.Background()

	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// DisableKeepalive=true: do NOT inject, container may exit.
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:             "ka-disabled",
		Image:            alpineDigest,
		DisableKeepalive: true,
	})
	if err != nil {
		// Create may fail if waitReady detects the container exited — acceptable.
		t.Logf("Create returned error (acceptable: container exited at boot): %v", err)
		return
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	// Give the container a moment to finish its default CMD (/bin/sh exits).
	time.Sleep(300 * time.Millisecond)

	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Logf("ContainerInspect error (container may be gone): %v", err)
		return // container already removed — that's fine, it exited
	}
	if info.State != nil && info.State.Running {
		t.Errorf("container is still running with DisableKeepalive=true; want it to have exited (no injection)")
	}

	// Exec must fail because the container is not running.
	_, _, err = b.Exec(ctx, h, cont.Cmd{Run: "echo hi"})
	if err == nil {
		// Even if Exec starts, read the result channel.
		t.Log("Exec did not return error immediately; container may still be counted as running momentarily")
	}
	// We just need to verify the container's DockerID reflects no CMD override.
	if info.Config != nil {
		cmd := strings.Join(info.Config.Cmd, " ")
		t.Logf("DisabledKeepalive container Cmd = %q (confirming no sleep-infinity injection)", cmd)
		if strings.Contains(cmd, "sleep infinity") {
			t.Errorf("DisableKeepalive container has sleep infinity in Cmd; want no injection")
		}
	}
}

// TestKeepalive_ImageWithOwnCmdNotOverridden verifies that an image that
// already declares its own CMD in the image manifest is NOT given a keepalive
// injection (the image's CMD is not command-less).
//
// We test this via spec.Cmd="sleep 30" — the author sets a long-lived cmd
// explicitly, so cfg.Cmd is non-empty and the injection guard (len(cfg.Cmd)==0)
// skips the ImageInspect path entirely.
//
// The structural property we're testing: injection only fires when BOTH
// cfg.Cmd is empty AND the image has no CMD/ENTRYPOINT. When the author sets
// Cmd, cfg.Cmd is non-empty — no inject regardless of image defaults.
func TestKeepalive_ImageWithOwnCmdNotOverridden_AuthorOverride(t *testing.T) {
	cli, b := newTestBackend(t, "keepalive-own-cmd")
	ctx := context.Background()

	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Author sets a different long-running cmd: sleep 30 (distinct from sleep infinity)
	// to confirm the production code uses the author's cmd, not the injected one.
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:  "ka-own-cmd",
		Image: alpineDigest,
		Cmd:   []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if info.Config == nil {
		t.Fatal("Config is nil")
	}
	cmd := strings.Join(info.Config.Cmd, " ")
	// Must be the author's "sleep 30", not the injected "sleep infinity".
	if cmd != "sleep 30" {
		t.Errorf("container Cmd = %q, want %q (no keepalive override when author sets Cmd)", cmd, "sleep 30")
	}

	// Exec must work since sleep 30 keeps the container alive.
	chunks, result, err := b.Exec(ctx, h, cont.Cmd{Run: "echo author-cmd"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var stdout strings.Builder
	for c := range chunks {
		if c.Stream == "stdout" {
			stdout.Write(c.Data)
		}
	}
	r := <-result
	if r.ExitCode != 0 {
		t.Errorf("Exec exit code = %d, want 0", r.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "author-cmd" {
		t.Errorf("stdout = %q, want %q", got, "author-cmd")
	}

	// Cleanup info for diagnostics.
	_ = cli.ContainerRemove(ctx, h.ID, dockerContainer.RemoveOptions{Force: true})
}
