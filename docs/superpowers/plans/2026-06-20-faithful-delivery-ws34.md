# Faithful Delivery WS-3 + WS-4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** WS-3 — docker injects a default keepalive for a command-less single image (dissolving the customer's native-fallback cascade), with author `cmd:`/`keepalive:` controls; gated on a man-page readiness revision. WS-4 — three validate/adapter hardening changes: a misplaced-`with:`-key warning (AWF1061), a shell-host injection lint (AWF3013), the `</thinking>` deterministic tag-strip, and a credential-presence reachability preflight.

**Architecture:** WS-3 is additive: `ContainerSpec.Cmd` is already plumbed end-to-end; we add the IR `cmd:`/`keepalive:` fields, thread them in `ContainerSpecFor`, and add **inspect-first** keepalive injection in the docker backend (`ImageInspect` → inject `["sleep","infinity"]` only when the image declares no Cmd/Entrypoint and the author set no `cmd:`/disabled keepalive). WS-4 adds two pure-IR validate passes, one shared `agent` string helper wired into three adapters, and one CLI-side advisory guard behind a new optional adapter interface.

**Tech Stack:** Go 1.26, single binary `awf`. No new third-party dependencies (the `</thinking>` fix is a hand-rolled tag-strip; `jsonrepair` is deliberately NOT added).

## Global Constraints

- **Go ≥ 1.26.2.** Pre-commit gate is **`make lint test`** (gofmt + go vet + golangci-lint, then `go test -race ./...`).
- **WS-3 Task 3 (keepalive) needs real Docker** — the docker backend isn't unit-mockable, so its test is `integ`-tagged (build tag `integ`) and runs under **`make integ`** (Docker/podman required; per project setup that is the cve-runner). The implementer verifies `make lint test` compiles + unit-passes and writes the integ test; the container-stays-alive assertion is verified under `make integ`. All other tasks are fully verified by `make lint test`.
- **AWF diagnostic codes (assigned here):** **AWF1060** = `cmd:`/`keepalive:` declared on a non-image-mode container; **AWF1061** = a reserved step-level key name nested inside `with:`; **AWF3013** = string-typed ref substituted unquoted into a `run:`/`idempotency_key` shell host. Each MUST be added to the `catalog` map in `ir/diagnostic.go` before its emit site references it. `TestCatalogCodesAreUnique` enforces the shape.
- **Invariants (AGENTS.md):** keepalive injection must be **inspect-first** — never override an image's own `Cmd`/`Entrypoint` (the `76802de` rationale + Phase-4 §A); `with:` stays opaque to the *engine* (a validate-time literal key-name scan is allowed, per the existing AWF1057/1058 precedent — do not interpret key *meaning*); the credential guard is **advisory (Warning), non-fatal** — it must NOT hard-reverse the deliberate "missing env silently omitted, auth fails at Launch" decision; adding `cmd:` to `ir.Container` changes the definition digest for workflows that use it (expected — it is part of the pinned definition).
- **Man-page rule:** the readiness revision (Task 1) documents only behavior that ships on THIS branch; it merges atomically with the code. Use the `updating-the-manual` discipline — verify the revised claims with an independent refuter and run `make man`.
- **Commit style:** conventional commits; end each body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Branch:** `worktree-faithful-delivery-ws34` (off `d830618`, includes WS-1/WS-2). `docs/` is git-ignored → docs commit with `git add -f`; code commits normally.

---

## Task 1: WS-3 man-page readiness/keepalive revision (the gate)

**Files:** Modify `man/awf-workflow.5.md` (readiness paragraph ~228-232; CONTAINERS field list ~205; the YAML example ~175-177). Then `make man`.

This ships with the code (Tasks 2-3) on the same branch, so `main` never describes unshipped behavior.

- [ ] **Step 1: Replace the readiness paragraph** (`man/awf-workflow.5.md:228-232`) with:

```
Readiness is re-established on every (re)creation, including resume. The runtime
guarantees a container is healthy before dispatching a step into it. A Compose
project becomes ready via its services' healthchecks and `up --wait`. A
single-image container becomes ready via its own command when it declares one
(an entrypoint/CMD baked into the image, or an author **cmd**); a single image
with no long-running command of its own would exit the instant it boots, so the
runtime injects a default **keepalive** (a `sleep`-forever command) to hold it
open for steps to exec into — the standard devcontainer `overrideCommand`
behavior. The keepalive applies only to a command-less single image: declare
**cmd** to run the image's own command instead (e.g. a server the steps talk
to), or **keepalive: false** to inject nothing and let a self-exiting image
exit. There is deliberately no `setup` step and no per-step "re-run on resume"
flag.
```

- [ ] **Step 2: Add the `cmd`/`keepalive` definition-list entries** after the `resources.cpu`/`resources.mem` block (~`man/awf-workflow.5.md:205`):

```
**cmd**
:   Optional, image-mode only. The command run inside a single-image container,
    overriding the image's CMD. Given as a list of arguments:

        containers:
          api:
            image: oci://registry.example.com/api@sha256:abc...
            cmd: [ "/usr/bin/api", "--serve" ]   # the server the steps exec against

    When omitted, the image's own entrypoint/CMD runs; if the image declares no
    long-running command, the runtime injects a keepalive so the container stays
    up (see READINESS). `cmd` has no meaning with `compose` — a Compose service's
    command is declared in the Compose file.

**keepalive**
:   Optional, image-mode only, default `true`. When the runtime would otherwise
    inject a keepalive into a command-less single image, **keepalive: false**
    suppresses it. Ignored when `cmd` or an image entrypoint/CMD is present, and
    meaningless with `compose`.
```

- [ ] **Step 3: Add the `cmd:` line to the CONTAINERS YAML example** (`man/awf-workflow.5.md:~175-177`, the `scratch:` block): `cmd: [ "/bin/sh", "-c", "sleep infinity" ]   # optional; default keepalive if omitted`

- [ ] **Step 4: Independent refute + render.** Dispatch a fresh refuter (per `updating-the-manual`) to check each revised claim against the code anchors (`container/docker/backend.go` Create, `ir/types.go` Container, `ContainerSpecFor`). Fix anything flagged. Then `make man` and confirm both pages render (definition lists as tagged paragraphs, YAML indentation intact).

- [ ] **Step 5: Commit** (`git add -f man/awf-workflow.5.md`):
```
docs(man): readiness contract — runtime injects a keepalive for command-less images
```
(body: describes the WS-3 contract; ships with the code on this branch; Co-Authored-By trailer)

---

## Task 2: WS-3 IR — `cmd:`/`keepalive:` fields + threading + AWF1060

**Files:** `ir/types.go` (Container struct), `engine/local_dispatcher.go` (`ContainerSpecFor` ~347-387), `container/backend.go` (update the stale `ContainerSpec.Cmd` doc comment ~224-231), `ir/validate_structural.go` (AWF1060, container switch ~45-78), `ir/diagnostic.go` (catalog), and tests in `ir/validate_structural_test.go` + `engine/local_dispatcher_test.go` (or the nearest `ContainerSpecFor` test).

**Interfaces:** `ir.Container` gains `Cmd []string json:"cmd,omitempty"` and `Keepalive *bool json:"keepalive,omitempty"`; `ContainerSpecFor` sets `spec.Cmd = c.Cmd` (image-mode arm).

- [ ] **Step 1: Failing AWF1060 test** — add to `ir/validate_structural_test.go` a case: a `compose:`-mode container that also declares `cmd:` → expect AWF1060. (Mirror the existing container-switch tests around AWF1005-1025.) Run RED.

- [ ] **Step 2: Add the IR fields** (`ir/types.go:47-54`):
```go
type Container struct {
	Image     string     `json:"image,omitempty"`
	Compose   string     `json:"compose,omitempty"`
	Service   string     `json:"service,omitempty"`
	Snapshot  string     `json:"snapshot,omitempty"`
	Cmd       []string   `json:"cmd,omitempty"`
	Keepalive *bool      `json:"keepalive,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}
```

- [ ] **Step 3: AWF1060 validation** — in `ir/validate_structural.go`'s container switch, after the image/compose determination, emit AWF1060 when `c.Compose != "" && (len(c.Cmd) > 0 || c.Keepalive != nil)` (cmd/keepalive are image-mode only). Add the catalog entry in `ir/diagnostic.go` (AWF1xxx block):
```go
	"AWF1060": "cmd:/keepalive: is image-mode only; it has no meaning on a compose: container (a service's command lives in the Compose file)",
```

- [ ] **Step 4: Thread into `ContainerSpecFor`** (`engine/local_dispatcher.go`, image-mode arm) — add `spec.Cmd = c.Cmd` after `spec.Image = c.Image`. Update the now-stale doc comment in `container/backend.go:224-231` (drop "Today's engine.ContainerSpecFor never populates Cmd").

- [ ] **Step 5: GREEN + a `ContainerSpecFor` unit test** asserting a Container with `Cmd: ["x"]` yields `spec.Cmd == ["x"]`. Run `go test ./ir/ ./engine/ -run "Structural|ContainerSpec" -v`.

- [ ] **Step 6: `make lint test` + commit** `feat(ir): cmd:/keepalive: container fields (image-mode), AWF1060`.

---

## Task 3: WS-3 docker inspect-first keepalive injection

**Files:** `container/docker/backend.go` (`Create`, after `cfg` is built ~233), and an `integ`-tagged test in `container/docker/` (mirror `backend_p6a_integ_test.go` / `exec_integ_test.go`). The `Keepalive *bool` opt-out and `cmd:` override are honored.

- [ ] **Step 1: Failing integ test** (build tag `integ`) — Create a command-less image (alpine, whose CMD is `/bin/sh`, exits immediately) via the **plain** `Backend.Create` (no `backendWithSleepInfinity` wrapper, no `spec.Cmd`), then assert the container stays running and `Exec` succeeds. Today this fails (container exits). Mirror the rig in `container/docker/exec_integ_test.go`. Run RED under `make integ` (or `go test -tags=integ ./container/docker/ -run <name>`).

- [ ] **Step 2: Inspect-first injection** — in `container/docker/backend.go` `Create`, after `cfg := &dockerContainer.Config{Image: spec.Image, Cmd: spec.Cmd}` and before `ContainerCreate`, add (honoring a `Keepalive` opt-out passed via the spec — see note):
```go
	// Default keepalive: a single image with no command of its own (and no
	// author cmd:) would exit at boot and Exec would fail "not running". Inject
	// `sleep infinity` ONLY as a last resort so an author cmd: and the image's
	// own CMD/ENTRYPOINT are both honored (Phase 4 design §A; reverses 76802de
	// only for the genuinely command-less case).
	if len(cfg.Cmd) == 0 && spec.Keepalive {
		inspect, ierr := b.cli.ImageInspect(ctx, spec.Image)
		if ierr != nil {
			return container.Handle{}, fmt.Errorf("container/docker: Create: ImageInspect: %w", ierr)
		}
		if inspect.Config == nil || (len(inspect.Config.Cmd) == 0 && len(inspect.Config.Entrypoint) == 0) {
			cfg.Cmd = []string{"sleep", "infinity"}
		}
	}
```

> **Keepalive opt-out plumbing:** `ContainerSpec` needs a way to carry the `keepalive: false` opt-out. Add `Keepalive bool` to `container.ContainerSpec` (default true means "inject if needed"). In `ContainerSpecFor` (Task 2's edit) set `spec.Keepalive = c.Keepalive == nil || *c.Keepalive` (absent/true → inject-if-needed; explicit false → never). Document the field. (If the implementer finds a cleaner carry, report it — but the opt-out must reach `Create`.) `ImageInspect` returns `image.InspectResponse` (the `image` package is already imported in `backend.go`); `inspect.Config` is the OCI config carrying `Cmd`/`Entrypoint` — no new import needed.

- [ ] **Step 3: GREEN under integ** (`make integ` or `go test -tags=integ ./container/docker/ -run <name>`); the command-less image now stays alive and `Exec` succeeds. An author-`cmd:` and an image-with-its-own-CMD path must NOT get the keepalive (add assertions). Snapshot/Restore round-trip is automatic (no edit).

- [ ] **Step 4: `make lint test`** (compile + unit; the keepalive assertion itself is integ). Commit `feat(container/docker): inject default keepalive for command-less images (inspect-first)`. **Report the integ result explicitly** (which command + output); if Docker is unavailable in the implementer's env, report BLOCKED on integ verification so the controller routes it to the cve-runner.

---

## Task 4: WS-4 misplaced-`with:`-key warning (AWF1061)

**Files:** new `ir/validate_misplaced_with.go` (pass `validateMisplacedWithKeys`), register in `ir/validate.go` after `validateTools`, catalog entry in `ir/diagnostic.go`, test in `ir/validate_refs_test.go` (or a new `_test.go`).

- [ ] **Step 1: Failing test** — an `AgentStep` whose `with:` contains a key named `input_files` (or `output_schema`) → expect an AWF1061 **Warning** at that step. Mirror `TestRefsAgentSchemaWithoutRefWarnsAWF3002` (assert `d.Code=="AWF1061" && d.Severity==Warning`). Run RED.

- [ ] **Step 2: The pass** (`ir/validate_misplaced_with.go`, package `ir`) — walk `*AgentStep` nodes via `WalkNodes`; for each, scan `as.With` keys against a reserved denylist (the AgentStep step-level json names except `with`): `id, container, uses, continues, output_schema, output_files, skills, input_files, timeout, idempotency_key, retry`. Emit `c.warnf(path, "AWF1061", …)` per offending key. Mirror the `validateTools` `WalkNodes` + `r.With[...]` idiom. Catalog:
```go
	"AWF1061": "a reserved step-level key is nested inside with: (it will be ignored by the engine); move it to the step level (sibling of with:)",
```

- [ ] **Step 3: Register** in `ir/validate.go` after `validateTools(modLD, c)`; add the doc-comment bullet. **Step 4: GREEN + regression** (`go test ./ir/ -v`); confirm a legitimately-named with-key (e.g. `prompt`, `model`) does NOT warn. **Step 5: `make lint test` + commit** `feat(ir): warn on reserved step-level keys nested under with: (AWF1061)`.

---

## Task 5: WS-4 shell-host injection lint (AWF3013)

**Files:** `ir/validate_refs.go` (new `checkShellHostInjection` + `refIsStringTyped` + `slotIsShellQuoted` helpers; call from the `walkRefs` `*CodeStep`/`*AgentStep` arms at the `.run`/`.idempotency_key` sites), catalog entry, tests in `ir/validate_refs_test.go`.

- [ ] **Step 1: Failing test matrix** — `CodeStep{Run: "curl {{ step.a.url }}"}` with `a`'s schema `properties:{url:{type:string}}` → expect AWF3013 **Warning** at `.run` (unquoted string). And the no-warn cases: `Run: "curl \"{{ step.a.url }}\""` (quoted), an `integer`/`exit_code` field (number → safe), and a `with.<k>` ref (non-shell host → no warn). Use `assertWarningAt`/`assertNoCode` helpers. Run RED.

- [ ] **Step 2: The lint** — add `checkShellHostInjection(src, path, c, producers)` (re-scan with `template.Slots` to get byte offsets; for each slot: parse the ref, skip if not string-typed via the producer-schema `properties[field].type=="string"` lookup, skip if shell-quoted via a deliberately-simple `host[sl.Start-1]`/`host[sl.End]` quote-char heuristic, else `c.warnf(path, "AWF3013", …)`). Call it in `walkRefs` ONLY at the `*CodeStep` `.run` site and the `.idempotency_key` sites (NOT `with.<k>`/`skills.query`). Catalog:
```go
	"AWF3013": "string-typed reference substituted unquoted into a run:/idempotency_key shell host; an attacker-controlled value can inject shell commands (CWE-78) — wrap the slot in double quotes",
```
Document the quote heuristic as deliberately surface-level (mirror `validateAwfOutputWrites`'s stance) — number/bool slots are safe (skip), quoted slots are skipped; a residual false-negative on exotic quoting is accepted.

- [ ] **Step 3: GREEN + scan goldens** — run `go test ./ir/ -v`; check `loader/testdata`/`ir/testdata` for `run:` hosts with unquoted string refs that would newly warn, and regenerate any affected `.golden` (Warning-only; `TestValidFixturePassesClean` only asserts zero Errors). **Step 4: `make lint test` + commit** `feat(ir): lint unquoted string refs in run:/idempotency_key shell hosts (AWF3013, CWE-78)`.

---

## Task 6: WS-4 `</thinking>` deterministic tag-strip

**Files:** new `agent.StripThinkTags` in the `agent` package (e.g. `agent/jsonextract.go`), wired into `extractJSONObject` in `agent/awfllm/stream.go`, `agent/droid/stream.go`, `agent/goose/stream.go` (one call-line each). Test the shared helper + one per-adapter parse test.

- [ ] **Step 1: Failing test** — `agent.StripThinkTags("<think>reasoning</think>{\"ok\":true}")` returns `{"ok":true}`; and an awfllm-level test that `ExtractJSONObjectForTest("…</thinking>{\"verified\":true}")` (and `</think>`) parses (today it fails — the brace-scan finds the object, BUT a reasoning block containing a `{` would mis-parse; the tag-strip removes that risk). Run RED. **Decision: hand-rolled, no `jsonrepair`** (the adapters' own comments reject it).

- [ ] **Step 2: The shared helper** (`agent/jsonextract.go`, package `agent`):
```go
// StripThinkTags removes a leading reasoning block delimited by a closing think
// tag (</think> or </thinking>), keeping only the text AFTER the LAST such tag —
// reasoning models emit "...reasoning...</think>{json}". If no closing tag is
// present, s is returned unchanged (the brace-scan handles tag-less output).
func StripThinkTags(s string) string {
	for _, tag := range []string{"</thinking>", "</think>"} {
		if i := strings.LastIndex(s, tag); i >= 0 {
			return s[i+len(tag):]
		}
	}
	return s
}
```
(Use the LAST tag — vLLM's first-tag partition breaks multi-block output. Imports: `strings`.)

- [ ] **Step 3: Wire into the three adapters** — in each `extractJSONObject`, change the first line `s = stripJSONFence(strings.TrimSpace(s))` to `s = stripJSONFence(agent.StripThinkTags(strings.TrimSpace(s)))`. (The new logic lives ONCE in `agent.StripThinkTags`; only a one-line call is added to each pre-existing copy — no new duplication.) Confirm each adapter already imports `github.com/valbaudo/awf/agent`.

- [ ] **Step 4: GREEN** (`go test ./agent/... -v`). codex/codexlive (strict, native-schema) and claude (native `structured_output`) are intentionally untouched. **Step 5: `make lint test` + commit** `feat(agent): strip reasoning tags before JSON extraction (awfllm/droid/goose)`.

---

## Task 7: WS-4 credential-presence reachability preflight

**Files:** new optional interface `CredentialNamer` in `agent/adapter.go`; implement `RequiredEnv()` on the credential-bearing adapters (awfllm, codex, codexlive, goose, droid, claude — each already lists its env names in its `errors.go`); new `cli/credential_guard.go` (`checkCredentialPresence…`), called in `cli/run.go` at the Part-D guard band (~254-264, before `run.started`) and in `cli/resume.go` near `preflightLiveResume`. Advisory **Warning**, non-fatal.

- [ ] **Step 1: Failing test** — a CLI-level (or guard-unit) test: a workflow using an adapter whose required env is unset produces an advisory warning naming the adapter + the missing var, and the run still proceeds (non-fatal). Mirror `cli/threaded_guard.go`'s test pattern. Run RED.

- [ ] **Step 2: The interface** (`agent/adapter.go`, optional like `ResumePreflighter`):
```go
// CredentialNamer is an optional Adapter interface: RequiredEnv returns the
// host env var names this adapter needs to authenticate. Used by the run-start
// credential-presence preflight to WARN (not fail) when a required var is
// absent — surfacing a likely Launch failure before any container boots, while
// preserving the "missing env is silently omitted; auth fails at Launch"
// contract (this is advisory only).
type CredentialNamer interface {
	RequiredEnv() []string
}
```

- [ ] **Step 3: Implement `RequiredEnv()`** on each credential-bearing adapter, returning the names already defined in its `errors.go` (awfllm: OPENAI_API_KEY, ANTHROPIC_API_KEY; codex/codexlive: OPENAI_API_KEY; goose: ANTHROPIC_API_KEY/OPENAI_API_KEY per provider; droid: FACTORY_API_KEY; claude: its key family). Return the adapter's existing env-name vars — do not hardcode new strings.

- [ ] **Step 4: The guard** (`cli/credential_guard.go`) — mirror `cli/threaded_guard.go`: walk the resolved agent steps, `resolver.Lookup` each `uses`, type-assert `agent.CredentialNamer`, and for each name not present in the host env (`os.LookupEnv`), append a warning line. Print all warnings to stderr; return nil (never an error). Call it in `cli/run.go` after the resolver is built and before `run.started`, and in `cli/resume.go`. **Advisory only.**

- [ ] **Step 5: GREEN** (`go test ./cli/ ./agent/... -v`). **Step 6: `make lint test` + commit** `feat(cli): advisory credential-presence preflight (CredentialNamer)`.

---

## Self-Review

- **Coverage:** WS-3 = T1 (man gate) + T2 (IR fields + AWF1060) + T3 (keepalive injection). WS-4 = T4 (AWF1061) + T5 (AWF3013) + T6 (tag-strip) + T7 (reachability). No design item deferred ("no defers").
- **Code uniqueness:** AWF1060/1061/3013 distinct; the tag-strip logic lives once in `agent.StripThinkTags`; the credential guard mirrors `threaded_guard.go` (not duplicated logic).
- **Type consistency:** `ir.Container.Cmd []string` / `Keepalive *bool`; `ContainerSpec.Keepalive bool`; `ContainerSpecFor` sets both; `CredentialNamer.RequiredEnv() []string`.
- **Sequencing:** T1 (man) precedes T2-T3 (code) but ships on the same branch. T2 before T3 (T3 reads `spec.Keepalive` from T2's threading). T4-T7 independent of each other and of WS-3.

## Verification notes / known constraints

- **T3 keepalive is integ-only** (real Docker). The implementer verifies `make lint test` + writes the integ test; the live assertion runs under `make integ` (cve-runner). If Docker is unavailable locally the implementer reports BLOCKED-on-integ so the controller routes the integ run.
- **T7 is advisory by design** — a Warning, not a fatal error, so it does not reverse the deliberate "auth fails at Launch" decision; it only surfaces the likely failure earlier. A paid `GET /models` probe is explicitly out of this surface (no free Adapter seam; would need a new interface + per-adapter HTTP) — credential-presence is the reachability signal that ships.
