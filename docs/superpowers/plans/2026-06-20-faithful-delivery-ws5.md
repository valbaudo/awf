# Faithful Delivery WS-5 (native isolation) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]`.

**Goal:** Make AWF's native (no-Docker) backend safe and functional: (a) **staging-workdir-relative** so AWF-side staging (reduce manifest/branch, react args) works on native instead of writing literal `/work` to the host (the customer's original native blocker); (b) **child-process sandbox** so each native step READS host login creds but CONFINES WRITES to its per-run workdir/scratch — **bubblewrap** (Linux, default) → **go-landlock re-exec trampoline** (Linux, fallback) → **sandbox-exec** (macOS), with a loud no-isolation fallback when none is available.

**Architecture:** AWF spawns each native step as `exec.CommandContext(ctx,"sh","-c",run)` with `c.Dir=r.workdir` and host env (`container/native/exec.go:49-51`) — the single argv site to prefix a sandbox launcher. The launcher is chosen per-OS via `exec.LookPath` detection with a fallback chain. go-landlock can't sandbox a child in-process (it restricts the caller), so the fallback uses a hidden `awf __sandbox` re-exec trampoline that applies Landlock then `exec`s the step. Staging uses a new `container.Caps.StagingRoot` (docker `/work/.awf` unchanged; native workdir-relative `.awf`) + an injected `AWF_STAGING_ROOT` env so reducer `run:` is backend-portable.

**Tech Stack:** Go 1.26 single binary. **New dep:** `github.com/landlock-lsm/go-landlock/landlock` (the Linux fallback; approved). bubblewrap + sandbox-exec are runtime tools (detected, not bundled). Cross-platform via `*_linux.go`/`*_darwin.go` build tags.

## Global Constraints
- **Go ≥ 1.26.2.** Gate: `make lint test` (compile + unit + race; the fake backend ignores sandboxing).
- **VERIFICATION SPLIT (this host is macOS):** the macOS `sandbox-exec` backend + ALL pure-Go (staging, detection, arg-construction unit tests) are verified by `make lint test` + a darwin integ test HERE. The **Linux bubblewrap + go-landlock backends are compile+unit-verified only here**; their live integ (does the child actually confine?) runs under `make integ` on a Linux box (cve-runner/CI). Mark Linux sandbox integ tests `//go:build integ && linux` and report them as cve-runner-pending, NOT as passed here.
- **Invariants:** the backend (not the engine) owns exec — the sandbox prefix lives in `container/native`, never the engine. `container.Cmd` stays `{Run, Env}` (no Argv). Do NOT introspect backend type in the engine — use `container.Caps` (the staging root is a declarative Cap, per `engine/local_dispatcher.go:212` "does NOT introspect the Backend type"). Fail-closed: if a sandbox tool is configured-required but absent, error or downgrade EXPLICITLY with a loud durable warning — never silently run unsandboxed.
- **Reads allowed, writes confined** is the contract: host cred dirs stay READABLE (so logged-in agent CLIs work); writes confined to the per-run workdir + scratch + /tmp. This is NOT a hardware boundary (right for a misbehaving-not-malicious agent).
- Two man revisions (ship with the code): §3.2a staging (`$AWF_STAGING_ROOT`) and the BACKENDS/native section (native write-confines via an OS sandbox). Use `updating-the-manual` discipline.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. `docs/` force-added.

---

## Task 1: WS-5 staging-workdir-relative (the dependency-free native /work fix)

**Files:** `container/types.go` (the `Caps` struct — add `StagingRoot string`), `container/native/backend.go` (`Capabilities()` `:128-137` → `StagingRoot: ".awf"` workdir-relative), `container/docker/backend.go` (`Capabilities()` → `StagingRoot: "/work/.awf"` unchanged), `engine/reduce.go` (`:22-29` consts → derive from `ld.Backend.Capabilities().StagingRoot`; manifest dst `:237`; branch dst `:253`; inject `AWF_STAGING_ROOT` into the reducer `ResolvedInputs.Env` `:286-293`), `engine/react.go:574` (args-file root from the same cap), `man/awf-workflow.5.md:1017-1027` (§3.2a — document `$AWF_STAGING_ROOT`), tests (`engine/reduce_test.go`, `engine/react_test.go`, `conformance/` literal assertions).

> **Scope note:** `into:` skills staging is OUT of this task — `into:` is author-facing and validator-locked to absolute (`ir/validate_skills.go:100`); fixing it for native needs a separate format decision (relax the absolute-`into:` validator). Reduce + react use engine-internal hardcoded `/work/.awf`, which the engine can change freely — that's this task. Flag `into:`-on-native as a follow-up.

- [ ] **Step 1: Failing test** — a native-backend reduce run where the reducer reads `$AWF_STAGING_ROOT/aggregate.json` resolves (today the manifest lands at literal host `/work/.awf` and the reducer can't read it). Assert the manifest is staged under the per-run workdir on native, and `AWF_STAGING_ROOT` is in the reducer step env; assert docker still stages `/work/.awf`. Run RED.
- [ ] **Step 2: Add `Caps.StagingRoot`** to `container/types.go` `Caps`; set docker `/work/.awf`, native `.awf` (workdir-relative — `CopyTo` already joins relative paths to `r.workdir`, so no copy.go change).
- [ ] **Step 3: Engine wiring** — `engine/reduce.go`: build manifest/branch dsts from `ld.Backend.Capabilities().StagingRoot` (keep `/work/.awf` as the docker value); inject `"AWF_STAGING_ROOT": <root>` into the reducer `ResolvedInputs.Env`. Same cap-driven root for `engine/react.go:574` args file.
- [ ] **Step 4: man §3.2a** — document `$AWF_STAGING_ROOT` as the reducer-staging contract (docker value `/work/.awf`; native workdir-relative). `make man`.
- [ ] **Step 5: GREEN** — update the literal-asserting tests (`reduce_test.go`/`react_test.go`/`conformance/skills.go`) to the cap-driven value (docker default stays `/work/.awf`; native asserts the relative root). `go test ./engine/ ./conformance/ -run "Reduce|React|Map" -v` + `make lint test`.
- [ ] **Step 6: Commit** `feat(engine,container): per-backend staging root + AWF_STAGING_ROOT so native staging works (WS-5)`.

---

## Task 2: WS-5 sandbox seam — detection, fallback chain, Caps, CLI wiring (cross-cutting scaffold)

**Files:** `container/native/backend.go` (a `WithSandbox`/sandbox-enable Option alongside `WithBlobs` `:96-98`; store on Backend), `container/native/sandbox.go` (NEW, build-tag-free: the launcher interface + detection + fallback chain + cred-dir set + the no-op/warn fallback), `container/types.go` (a `Caps` sandbox field if needed for obs), `cli/backend.go:40-48` (pass the sandbox option to `native.New`), a cred-dir enumerator, tests.

**Interfaces:** `sandboxLauncher` — given `(scratchDir string, roDirs []string)` returns the argv prefix (`[]string`) to prepend to `["sh","-c",run]`, or nil (no-op). `detectSandbox() (sandboxLauncher, string)` returns the chosen launcher + a human label, via `exec.LookPath` (mirror `agent/codexlive/process_client.go:49`). Cred dirs from each adapter's config-dir (`~/.claude`, `$CODEX_HOME`/`~/.codex`, `~/.factory`, `~/.config/goose`, `~/.config`) + the per-run HOME.

- [ ] **Step 1: Failing tests** — `detectSandbox` returns the macOS launcher when `sandbox-exec` is present (darwin), the bwrap launcher when `bwrap` present (linux), the landlock-trampoline launcher when only the binary itself is available (linux fallback), and a nil/no-op + warn when none. The cred-dir enumerator returns the expected dirs. (Detection is mockable via a `lookPath func(string)(string,error)` seam — don't shell out in unit tests.) Run RED.
- [ ] **Step 2: Implement the seam** — `sandbox.go` (build-tag-free): the `sandboxLauncher` interface, `detectSandbox` with an injectable lookPath, the fallback chain (Linux: bwrap → landlock-trampoline → no-op+warn; macOS: sandbox-exec → no-op+warn; other: no-op+warn), the cred-dir enumerator (reuse each adapter's known config dir; resolve `os.UserHomeDir`). Wire `native.WithSandbox(enabled)` + `cli/backend.go` to enable it for the native backend; emit the loud durable no-isolation warning when the launcher is nil (mirror the existing native no-isolation warning).
- [ ] **Step 3: GREEN + `make lint test` + commit** `feat(container/native): sandbox launcher seam + detection/fallback chain (WS-5)`.

> The per-OS launcher implementations are Tasks 3 (Linux) + 4 (macOS), behind build tags. This task is the OS-agnostic plumbing + the no-op fallback (so the build is green on every platform before the backends land).

---

## Task 3: WS-5 Linux backends — bubblewrap (default) + go-landlock trampoline (fallback)

**Files:** `container/native/sandbox_linux.go` (NEW, `//go:build linux`: the bwrap launcher argv builder + the landlock-trampoline launcher), `cmd/awf/main.go` (the `__sandbox` re-exec trampoline, before `cli.Run`), `container/native/sandbox_landlock_linux.go` (the go-landlock apply), `go.mod`/`go.sum` (add go-landlock), tests (arg-construction unit tests; `//go:build integ && linux` live tests = cve-runner-pending).

**bubblewrap launcher** (the grounded argv recipe; use `--ro-bind-try` for maybe-missing cred dirs; ORDER: `--tmpfs $HOME` before the cred `--ro-bind`s):
```
bwrap --tmpfs <HOME> --ro-bind-try <credDir> <credDir> ... --bind <scratch> <scratch>
      --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind /lib /lib --ro-bind-try /lib64 /lib64
      --ro-bind /etc /etc --tmpfs /tmp --proc /proc --dev /dev --chdir <scratch>
      --unshare-pid --die-with-parent --new-session -- sh -c <run>
```
**go-landlock trampoline:** the native launcher returns `[self, "__sandbox", <policyJSON>, "--"]` (self = `os.Executable()`); `cmd/awf/main.go` `func main()` checks `os.Args[1]=="__sandbox"` BEFORE `cli.Run`, decodes the policy, calls `landlock.V9.RestrictPaths(RODirs(credDirs+system...).IgnoreIfMissing(), RWDirs(scratch,"/tmp").IgnoreIfMissing())` (NOT `BestEffort()` here — if Landlock is unavailable the trampoline must FAIL so detection falls back to no-op+warn; surface the error), then `syscall.Exec("/bin/sh", ["sh","-c",run], env)`.

- [ ] **Step 1: Failing arg-construction unit tests** (no Docker/kernel needed) — assert the bwrap argv has `--tmpfs $HOME` before the cred `--ro-bind-try`s, `--bind <scratch>`, `--chdir <scratch>`, and ends `-- sh -c <run>`; assert the trampoline argv is `[self,"__sandbox",<json>,"--","sh","-c",run]` and the policy JSON round-trips (RODirs=cred+system, RWDirs=scratch+/tmp). Run RED.
- [ ] **Step 2: Implement** — `sandbox_linux.go` (both launchers), `cmd/awf/main.go` trampoline (decode + go-landlock + syscall.Exec), add go-landlock to go.mod (`go get github.com/landlock-lsm/go-landlock/landlock`, `go mod tidy`). The trampoline subcommand must run BEFORE any CLI parsing and must NOT construct a Runner.
- [ ] **Step 3: Linux live integ tests** (`//go:build integ && linux`) — a bwrap run + a landlock-trampoline run where a step writes to host `~/.factory/x` → DENIED, and to `<scratch>/x` → OK, and reads `~/.factory` → OK. **Write them; do NOT claim they pass on this macOS host.** Mark them cve-runner-pending.
- [ ] **Step 4: GREEN on macOS = compile + arg-unit-tests pass** (`GOOS=linux go build ./...` to confirm the linux files compile; `make lint test` for the unit tests). **Report:** unit/compile GREEN here; Linux integ PENDING cve-runner.
- [ ] **Step 5: Commit** `feat(container/native): Linux child sandbox — bubblewrap + go-landlock trampoline (WS-5)`.

---

## Task 4: WS-5 macOS backend — sandbox-exec (verifiable here)

**Files:** `container/native/sandbox_darwin.go` (NEW, `//go:build darwin`: the sandbox-exec launcher), tests (`//go:build integ && darwin` live test — verifiable on THIS host).

**sandbox-exec launcher** — generate the SBPL per-invocation (allow-default reads, deny writes except scratch+TMPDIR), write to a temp `.sb`, prefix `["sandbox-exec","-D","SCRATCH="+scratchAbs,"-D","TMPDIR="+tmpdir,"-f",profilePath,"--"]`:
```scheme
(version 1)
(allow default)
(allow process-exec)
(allow process-fork)
(deny file-write*)
(allow file-write* (subpath (param "SCRATCH")))
(allow file-write* (subpath (param "TMPDIR")))
```

- [ ] **Step 1: Failing arg-construction unit test** — the launcher argv is `["sandbox-exec","-D","SCRATCH=<abs>",...,"--","sh","-c",run]` and the generated SBPL contains the deny-write + allow-write-scratch lines. Run RED.
- [ ] **Step 2: Implement** `sandbox_darwin.go` (profile gen + temp file + prefix). Honor the no-op fallback when `sandbox-exec` is absent (it ships with macOS, but guard anyway).
- [ ] **Step 3: macOS live integ test** (`//go:build integ && darwin`) — a native step that writes `$HOME/.factory/x` → DENIED, `<scratch>/x` → OK, reads `$HOME/.config` → OK. **This IS verifiable on this host:** run `go test -tags=integ ./container/native/ -run TestSandbox -v` and report the result.
- [ ] **Step 4: GREEN (incl. the macOS integ) + `make lint test` + commit** `feat(container/native): macOS child sandbox — sandbox-exec (WS-5)`.

---

## Task 5: WS-5 man BACKENDS/native revision

**Files:** `man/awf-workflow.5.md` (the `--backend native` / BACKENDS section — native now write-confines steps via an OS sandbox, reads host creds, falls back loudly when no sandbox tool is present), then `make man` + independent refute.

- [ ] **Step 1:** revise the native-backend prose: native runs steps on the host but **write-confines** each step via bubblewrap/go-landlock (Linux) or sandbox-exec (macOS) — host login config is readable, writes go to the per-run workdir/scratch; if no sandbox tool is available the run proceeds unsandboxed with a loud durable warning. Note `sandbox-exec` is Apple-deprecated (tracked fragility). **Step 2:** independent refute vs Tasks 2-4. **Step 3:** `make man` + commit `docs(man): native backend write-confines steps via an OS sandbox (WS-5)`.

---

## Self-Review
- T1 staging (pure-Go, fully verified). T2 sandbox seam+fallback (pure-Go). T3 Linux backends (compile+unit here; integ→cve-runner). T4 macOS backend (integ verified HERE). T5 man.
- The build stays green on every OS: the no-op fallback (T2) lands before the per-OS backends; the launcher is nil → no prefix → today's behavior + a loud warning.
- Fail-closed honored: a configured-required-but-absent sandbox tool → explicit downgrade + durable warning, never silent.

## Known constraints / deferred
- **Linux sandbox integ is cve-runner/CI-pending** (this host is macOS). Do not sign WS-5 off as fully verified until the Linux integ runs there.
- **`into:` skills on native** is deferred (validator-locked to absolute `into:`; needs a separate format relaxation). Reduce + react args are covered by T1.
- `sandbox-exec` is Apple-deprecated (functional through macOS 26.3); the only practical macOS option — tracked fragility.
- This is process-isolation, not a hardware boundary (right for the misbehaving-not-malicious threat model).
