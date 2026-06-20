# Task 3 Report: WS-3 docker inspect-first keepalive injection

## Status: DONE — all tests GREEN, committed

## Commit
`9a15988` — `feat(container/docker): inject default keepalive for command-less images (inspect-first)`

## Injection code as applied (backend.go ~212)

```go
// Default keepalive: a single image with no command of its own (and no
// author cmd:) would exit at boot and Exec would fail "not running". Inject
// `sleep infinity` ONLY as a last resort so an author cmd: and the image's
// own CMD/ENTRYPOINT are both honored (Phase 4 design §A; reverses 76802de
// only for the genuinely command-less case).
//
// Injection fires when the author did not supply a Cmd AND the image has no
// ENTRYPOINT. Images with an ENTRYPOINT declare a real service (e.g. a
// daemon binary) that is long-running; we must not override them. Images
// with no ENTRYPOINT but a shell CMD (e.g. alpine's /bin/sh) exit
// immediately when run non-interactively — they need the keepalive.
if len(cfg.Cmd) == 0 && !spec.DisableKeepalive {
    inspect, ierr := b.cli.ImageInspect(ctx, spec.Image)
    if ierr != nil {
        return container.Handle{}, fmt.Errorf("container/docker: Create: ImageInspect: %w", ierr)
    }
    if inspect.Config == nil || len(inspect.Config.Entrypoint) == 0 {
        cfg.Cmd = []string{"sleep", "infinity"}
    }
}
```

**Condition deviation from brief:** The brief specified `len(Cmd)==0 && len(Entrypoint)==0`. This was changed to `len(Entrypoint)==0` because alpine has CMD `/bin/sh` (non-empty) but no ENTRYPOINT — it still exits immediately without a TTY. The semantically correct guard is: "no ENTRYPOINT means we can safely override CMD with sleep infinity without breaking a real service". The brief's task description itself specified alpine as the test image for the "command-less" case, confirming that the `/bin/sh` CMD case must be covered.

## Integ test: container/docker/keepalive_integ_test.go

Four test functions:
1. `TestKeepalive_CommandlessImageStaysRunning` — happy path: alpine, no spec.Cmd, DisableKeepalive=false → sleep infinity injected → container running → Exec("echo hi") succeeds
2. `TestKeepalive_AuthorCmdNotOverridden` — negative (a): spec.Cmd set → cfg.Cmd non-empty → ImageInspect not reached → author's sleep runs
3. `TestKeepalive_DisabledDoesNotInject` — negative (b): DisableKeepalive=true → no inject → /bin/sh exits → Exec fails
4. `TestKeepalive_ImageWithOwnCmdNotOverridden_AuthorOverride` — author sets `sleep 30`; container Cmd confirmed as `sleep 30`, not `sleep infinity`

## RED run (before injection)

```
go test -tags=integ ./container/docker/ -run TestKeepalive_CommandlessImageStaysRunning -v -timeout 120s
--- FAIL: TestKeepalive_CommandlessImageStaysRunning (3.46s)
    keepalive_integ_test.go:50: container is not running after keepalive injection (State=&{Status:exited ...})
    keepalive_integ_test.go:56: Exec: container/docker: Exec: ContainerExecCreate: Error response from daemon: container ... is not running
FAIL
```

## GREEN run (after injection)

```
go test -tags=integ ./container/docker/ -run "TestKeepalive" -v -timeout 120s
=== RUN   TestKeepalive_CommandlessImageStaysRunning
--- PASS: TestKeepalive_CommandlessImageStaysRunning (1.91s)
=== RUN   TestKeepalive_AuthorCmdNotOverridden
--- PASS: TestKeepalive_AuthorCmdNotOverridden (1.60s)
=== RUN   TestKeepalive_DisabledDoesNotInject
    keepalive_integ_test.go:160: DisabledKeepalive container Cmd = "/bin/sh" (confirming no sleep-infinity injection)
--- PASS: TestKeepalive_DisabledDoesNotInject (2.03s)
=== RUN   TestKeepalive_ImageWithOwnCmdNotOverridden_AuthorOverride
--- PASS: TestKeepalive_ImageWithOwnCmdNotOverridden_AuthorOverride (1.74s)
PASS
ok  github.com/valbaudo/awf/container/docker  8.052s
```

## make lint test

`make lint test` — PASS. All 34 packages green. golangci-lint (gofmt + go vet + errcheck + staticcheck) clean.

## API notes

- `b.cli.ImageInspect(ctx, spec.Image)` — takes variadic `...ImageInspectOption` (no extra opts needed)
- `inspect.Config` is `*dockerspec.DockerOCIImageConfig` (embeds `ocispec.ImageConfig`)
- `inspect.Config.Entrypoint` is `[]string` from `ocispec.ImageConfig`
- `image` package already imported in backend.go — no new import needed

## Concerns

None. The condition deviation (Entrypoint-only vs Cmd+Entrypoint) is intentional and correct: the brief's alpine example requires it, and the semantics (images with an ENTRYPOINT have a real service) are sound.

---

## WS-3 Override-Default Correction (2026-06-20)

### Decision applied
Switched from inspect-first to **devcontainer overrideCommand default**: the runtime
injects `sleep infinity` whenever `spec.Cmd` is absent and `DisableKeepalive` is false
— unconditionally overriding the image's own CMD. No `ImageInspect` call.

### Changes
- `container/docker/backend.go`: removed `b.cli.ImageInspect` block; replaced with
  simple `if len(cfg.Cmd) == 0 && !spec.DisableKeepalive { cfg.Cmd = []string{"sleep","infinity"} }`.
  `image` import retained (used by `pullByDigest`).
- `container/docker/keepalive_integ_test.go`: updated doc comments to reflect
  override-default model (no ImageInspect language).
- `man/awf-workflow.5.md`: replaced readiness paragraph with override-default
  wording; updated `cmd` and `keepalive` field entries accordingly.

### Results
- `make lint test`: PASS (all 34 packages)
- `make man`: PASS (no errors)
- `go test -tags=integ ./container/docker/ -run TestKeepalive -v`: 4/4 PASS
