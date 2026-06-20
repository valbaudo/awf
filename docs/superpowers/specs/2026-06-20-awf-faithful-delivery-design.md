# AWF Faithful Delivery — closing the validate↔runtime contract gap

**Status:** design approved (brainstorming) + two adversarial review rounds + a code/web reality-check
+ a gap-methods research round (closed G1/G2, reduced G3); pending per-workstream specs.
**Date:** 2026-06-20
**Origin:** a customer ("prestige" offensive-security pipeline) shared four workflows + a 5347-line
debugging-session transcript in which `awf validate` reported all four **clean** on every pass, yet
the run failed ~16 times at runtime and required **two AWF engine source patches** plus four config
fixes before one end-to-end run completed (8.74M tokens). This is the exact class of failure AWF
exists to prevent. This doc is the design for preventing it.

Every claim below was verified against canonical source (file:line) and, where it concerns external
practice, against primary docs/SDKs (named inline).

---

## 1. The incident, in one paragraph

The customer's pipeline declared a digest-pinned image and a gate-bearing map. `awf validate` passed
clean (only "benign" `AWF3002` warnings, which the agent learned to dismiss wholesale). The run then
failed one wall at a time: the docker backend couldn't start the image (empty `Cmd`/`Entrypoint`, no
keepalive); the customer fell back to `--backend native`, which silently ran on the host, ignoring
the pinned image; native wrote `/work`, `/skills` literally to the host root (`mkdir: permission
denied`); a gate-in-map swallowed all 16 item failures with **no cause** (the real cause — a
skill-asset resolution bug — was invisible); a non-native-schema model leaked `</thinking>` around
its JSON and burned the retry budget; a `json.loads('{{ … }}')` step broke on quoting; `input_files`
nested under `with:` silently vanished; resume couldn't target the cheap failed step, forcing repeated
re-runs of a ~$9 parallel phase; and an agent corrupted its own host `~/.factory/settings.json`
because native inherits the host `HOME`. The customer's own agent named the root cause at transcript
line 2476: *"I've been debugging reactively instead of statically verifying the inter-step
contracts."*

## 2. Root cause

> **`awf validate` and the runtime enforce *different* contracts. `validate` is a pure static IR pass
> with no adapter, no backend, no host knowledge, so every contract that depends on those is deferred
> to *dispatch* — the most expensive moment, after the container is up and tokens are spent.**

The 14 confirmed problems are not 14 bugs. They are **three places AWF breaks one promise** — it
*substitutes* a different environment than declared, *swallows* the cause of a failure, and *forces*
the unsafe data-path to be the easy one — plus **one place it is needlessly un-incremental**, plus
**two genuine defects**. And one of the three — the entire native cascade (P4/P5/P6/P14) — only
happened because of a *single upstream defect*: the docker backend can't run a command-less image.

| # | Problem (confirmed, mainline) | Class | Change |
|---|---|---|---|
| P1 | `input_files`/`output_files` nested under `with:` silently vanish; validate passes | validate_gap | ⑤ |
| P2 | gate-in-map commits `item_failed` with no cause; obs never enriches it | runtime_swallow | ② |
| P3 | skill-library asset looked up by **bare id** not `QualifiedAssetKey(moduleID,…)` in sub-workflows | engine_bug | ⑥ |
| P4 | native writes absolute paths (`/skills`, `/work`) literally to host root | design_footgun | ① |
| P5 | reduce staging hardcoded `const reduceStagingDir = "/work/.awf"` | design_footgun | ① |
| P6 | `--backend native` silently ignores the declared pinned image | design_footgun | ① |
| P7 | docker backend injects no keepalive; empty-`Cmd` image dies with a cryptic crun error | design_footgun | ① (root) |
| P8 | no socket/volume/privileged in the container spec; dind works only via native host-leak | design_footgun | ① + doc |
| P9 | `NativeSchema=false` adapters can't enforce schema output; deterministic misbehavior burns retry | adapter_gap | ② + ⑤ |
| P10 | scalar-only templating forces `json.loads('{{ }}')`-into-shell; injection-fragile | design_footgun | ③ |
| P11 | a cheap step failing after an expensive committed parallel forces re-running it; edit→digest→re-run | design_footgun | ④ |
| P12 | `AWF3002` fires on every gate evaluator (false positive); trains users to ignore all warnings | validate_gap | ⑥ |
| P13 | no `(adapter,model,endpoint)` reachability check; some launch failures burn budget | design_footgun | ② + ⑤ |
| P14 | native inherits host `HOME`; an agent corrupted its own `settings.json` | design_footgun | ① |

## 3. Governing principle

> **AWF faithfully delivers what the workflow declares, and faithfully reports what it cannot deliver
> here. It never silently substitutes a different environment, never swallows a cause, and never
> forces the author to hand-build an unsafe path.**

Mechanism, not policy: AWF runs any environment you declare and moves any data you want — it refuses
only to *lie* about which one it did. A master stroke **removes** a problem class by subtraction or by
delivering the declared intent — it does not add a check per footgun (warning-fatigue: the customer
dismissing the whole `AWF3002` class is that failure mode, observed live).

## 4. Corrections baked in from the reality-check

Four earlier assumptions were overturned by code/web verification and are corrected throughout:

1. **Keepalive injection is the industry norm, not a violation.** The VS Code devcontainer spec
   defaults `overrideCommand: true`, injecting `/bin/sh -c 'while sleep 1000; do :; done'` *because*
   command-less images exit immediately; Codespaces inherits it, Testcontainers uses
   `tail -f /dev/null`. AWF's "never inject" stance (man:228-232) is the **outlier for an exec-target
   container**. → ① injects by default with an author override.
2. **There is no `--force` flag** (removed in `a89574f`); `--from` itself bypasses the digest pin
   (resume.go:166,252). → ④ corrected.
3. **The engine already classifies HTTP status from typed causes.** `codex/result.go:64-80` escalates
   `400/invalid_request` → permanent and keeps auth/429/5xx retryable (launch.go:205-212); the engine
   derives the outcome from typed adapter errors (local_dispatcher_agent.go:345-367). → ② is *extend
   the existing architecture*, not build a new one. The enum is `{ok, retryable_failure,
   permanent_failure, rejected}` (rejected = gate-only).
4. **Reading `with:` key-names does not break opacity.** `ir/validate_tools.go:77-84` already reads
   `with:` keys by literal name (AWF1057/1058). Opacity forbids interpreting key *meaning* with
   adapter semantics, not literal-name detection. → ⑤ is a pure `ir` literal scan.

---

## 5. The six changes

### ① Faithful provisioning — fix the docker keepalive first; that dissolves the native cascade
*Root-cause-dissolves P7; therefore P4, P5, P6, P14; plus P8 (doc).*

The customer's whole native disaster started because the **docker backend can't run a command-less
image**. Fix that and `auto` keeps them in docker (it already routes static-image→docker), and the
native footguns never fire.

- **Default keepalive, author override (devcontainer pattern).** When a container declares no command,
  the docker backend injects a keepalive (`sleep infinity`/`while sleep …`) — exactly
  `overrideCommand: true`. Add an author-facing `cmd:` (and optional `keepalive: false`) on
  `ir.Container` for images whose own command must run (e.g. a server the steps talk to). Plumbing is
  half-built: `ContainerSpec.Cmd` exists, docker `Create` honors it (backend.go:213), snapshot Restore
  re-applies it (snapshot.go:343); only an `ir.Container` field (ir/types.go:48-54) + one line in
  `ContainerSpecFor` + a validator + a §3/§readiness doc edit remain. Reverses the deliberate removal
  in `76802de`.
- **Native hardening (still relevant when there is genuinely no Docker):**
  - *Staging:* the reduce/skills staging the engine controls becomes **workdir-relative**, not literal
    `/work/.awf`. These are AWF-side Go file-ops (copy.go:39-49 already resolves *relative* paths
    under `r.workdir`); the fix is to default them relative. This is the customer's `AWF_STAGING_ROOT`
    patch, done as a relative-path default rather than an env knob. An *explicit* author-set absolute
    staging/output path on native (e.g. `into: /skills`) gets a **loud backend-aware validate warning**
    ("writes to host root on native; use a workdir-relative path") — it bind-remaps to scratch under
    bubblewrap and is denied (never silently lost to the host) under go-landlock/Seatbelt.
  - *Isolation (OS-native sandbox, not a HOME-swap):* write-confine each step with the same primitive
    every shipping agent CLI uses — **`landlock-lsm/go-landlock`** (pure-Go, in-process, daemonless) on
    Linux and **`sandbox-exec`/Seatbelt** (built into macOS) — restrict-then-exec from the dispatcher.
    Keep host cred dirs **readable** (`RODirs($HOME, /usr, /etc…)` / SBPL `allow default`) and **confine
    writes** to the per-run workspace + scratch + `/tmp` (`RWDirs(...)` / SBPL `deny file-write*` then
    re-`allow` under the workspace); `BestEffort()` no-ops on kernels without Landlock. This is what
    Codex CLI / Cursor / Gemini CLI (Landlock) and Claude Code / Codex (Seatbelt) ship today. It
    **replaces the HOME-swap idea and dissolves G1**: the agent reads the *real* `~/.factory`/`~/.codex`
    /XDG read-only and simply *cannot* corrupt them (fixes P14 at the source) — no fake HOME to seed. On
    Linux, **bubblewrap** (`--tmpfs $HOME` + `--ro-bind` cred dirs + `--bind $SCRATCH /work`) is the
    stronger alternative — it *hides* unbound paths and bind-remaps `/work`→scratch (also fixing P4's
    absolute-path write) — at the cost of an external binary + Linux-only. Optionally pair
    `elastic/go-seccomp-bpf` to cut per-run network egress (the Codex pairing). *Threat model:*
    process-isolation, not a hardware boundary — right for a misbehaving-not-malicious agent; the
    microVM tier (e2b/Firecracker/gVisor) is ruled out because it can't see the host creds the agent
    must read. To later *hide* a secret (`~/.aws`, `~/.ssh`), add an explicit `denyRead` (as Claude
    Code does). Env-var creds aren't covered by an FS sandbox — scope `cmd.Env` accordingly.
  - *Honesty:* when native genuinely ignores an image (no Docker), record it as a durable,
    interpreter-written fact in the log + `awf ls`/`inspect`, repeated on resume — never silent.

**Invariant interaction:** revises the man page readiness/keepalive contract (man:228-232) — required
format-standard edit, per the doc-hierarchy rule. **Lowest-risk, highest-leverage (it removes the
*reason* for five problems).**

### ② Richer typed causes — extend the existing classification; stop swallowing at fan-in
*Dissolves P2; refines P9/P13 classification; surfaces P3.*

This is **not a new mechanism** — the engine already derives `{ok|retryable|permanent}` from typed
adapter errors (local_dispatcher_agent.go:345-367), and codex already parses HTTP status
(result.go:64-80). The work is to make causes *richer and never dropped*:

- **Stop the fan-in swallow.** `engine/map.go` `dispatchItem` (507-521) drops the body error and
  commits `item_failed` with empty `Reason`. Add `Cause string json:"cause,omitempty"` to
  `MapItemData` (events.go:150), capture `bodyErr` at map.go:507-515, thread a **bounded, redacted**
  rendering through `commitMapItem` (keep `Reason` for the machine-readable infra sentinel). Reverse
  obs's "no map.item enrichment" (control_events.go:19-22) so `awf trace`/`inspect` show it. This
  enforces **crash ≠ verdict**: a mechanical body failure (e.g. P3's halt) is no longer
  indistinguishable from a tolerated rejection. **M3 closed:** the fold never hashes/equality-checks
  event `Data` (fold.go unmarshals into structured fields; pins only the workflow-file digest +
  runtime versions), so a free-text cause is purely informational and determinism-safe.
- **Generalize status→cause to droid/goose,** lifting codex's `result.go` pattern. Base map:
  `{transient: 408,409,429,5xx; permanent: 400,401,403,404,422}`; **do not** add a blanket
  "429→permanent" rule. Disambiguate the ambiguous 429 with a layered classifier (extend
  `isPermanentLLMError` in `agent/awfllm/stream.go`, which already gets Status+Type), top-down:
  (1) **`x-should-retry` header first** — authoritative for OpenAI/Anthropic (capture it; AWF keeps
  only Status/Type/Body today); (2) **body `type`/`code` on 429** — `insufficient_quota` /
  `budget_exceeded` / substrings "exceeded your current quota"·"check your plan and billing" →
  permanent, else transient; (3) **`Retry-After` present** → transient. Matches OpenAI/Anthropic SDK
  defaults + RFC 9110 §15.6.4 / RFC 6585 §4. **G3 caveat:** status alone is useless (both 429s; behind
  LiteLLM the budget case can be 400/401), so this matches a small `type`/`code` allowlist + message
  substrings — standard practice but brittle to provider wording changes.

**Invariant interaction:** none broken — strengthens crash≠verdict; outcomes stay mechanical-only.
**Lowest implementation risk.**

### ③ Safe structured data — warn + document; never auto-materialize
*Dissolves P10.*

Confirmed against code and the whole industry: **do not** make templating materialize composites.

- The template package is a pure leaf with zero `state.Blobs` access (CV6) — auto-materialization
  would require giving it a writer, breaking *interpreter-is-the-only-writer*. Keep the existing
  AWF4004 composite-rejection (template/eval.go:298-318) and the verbatim pre-shell scalar
  substitution.
- Interpolating data into a shell host is the **GitHub Actions CWE-78 script-injection class**; the
  cross-engine mitigation (GitHub Actions env-var/file, Argo artifacts-vs-parameters, Nextflow
  channels, Temporal typed payloads) is identical and is *already* AWF's contract: route structured
  data through `output_files` artifacts (man §587-593, 1368). No leading engine auto-materializes.
- **Add a lint warning** when a string `{{ step.*.<field> }}` reference is substituted into a shell
  host (`run:`/`idempotency_key:`), framed as the injection class — apt for an offensive-security tool.

**Invariant interaction:** none — additive, *consistent with* the existing templating contract (no
man-page templating revision needed).

### ④ Resume incrementality — small fix now; per-node addressing as a separate, gated spec
*Addresses P11.*

- **Now (small):** `--from` can't target the failed frontier node because `ResolveRerunTarget`
  (rerun.go:98-123) resolves committed-only and the fold ignores `node.failed` (events.go:450). But
  `failStep` durably records the path (`node.failed.Path`, interpreter.go:552) and
  `NodeFailedDataFromEvent` exists (events.go:567) — add one resolution arm that recovers the trailing
  failed-node path and feeds the existing `--from` plumbing.
- **Later (separate spec):** automatic per-node content-addressed reuse. The ambition is confirmed
  industry-standard — Bazel action-key, Buck2 action-digest, Nix CA-derivations + early cutoff,
  Turborepo task-hash; AWF's whole-workflow digest is the *exact* limitation Nix names for
  input-addressed derivations. **But** this touches *"pinning is a hard error on drift"* and its
  dominant failure mode is **non-hermetic/undeclared inputs → wrong cache hits (stale reuse)**, which
  is more dangerous than over-rebuilding (Build Systems à la Carte: minimality + early cutoff). It gets
  its own spec that must prove the declared-input set complete before trusting a subtree hash.
  **Crucially, content-reuse is sound only for DETERMINISTIC (code/shell) steps with declared inputs:
  agent steps are inherently non-hermetic — they use the network (LLM calls), the clock, and read host
  config, so no filesystem-hermeticity tool (Bazel/Nix sandbox, REAPI) can certify them — and are
  excluded from content-reuse (they keep today's exact commit/replay). Optional declared-vs-actual read
  linting (fsatrace / strace / fanotify / eBPF) is deferred.** **Not v1.**

**Invariant interaction:** the small fix touches none; the deferred spec revises §8 and must
re-justify the pinning invariant.

### ⑤ Validate as the honest last word — plus the schema ladder
*Recasts P1, P9; the one free pre-spend check for P13.*

- **Misplaced `with:` keys → warning (P1).** A `validateMisplacedWithKeys` pass in `ir` — a pure
  literal-name scan (like AWF1057/1058 at validate_tools.go:77-84) flagging reserved step-level names
  (`input_files`/`output_files`/`output_schema`/`skills`/…) inside any `With` map. No adapter, no
  registry, runs under `ir.Validate` (so `validate`, `run`, `resume` all surface it). Warning, not
  silent hoist (the engine must not depend on `with:` contents for behavior).
- **Schema conformance → a 3-tier ladder (P9).** (1) **Native constrained decoding** where the harness
  supports it (OpenAI Structured Outputs / Anthropic strict tool-use / Gemini `response_schema` /
  local GBNF·llguidance·Outlines) — AWF already wires this for `NativeSchema=true`. (2) **The real
  gemini-`</thinking>` fix: a deterministic adapter-internal extraction pipeline** (zero extra model
  calls), gated behind strict-parse-first and schema-validation-last: strict `Unmarshal` → on failure,
  strip to the **LAST** closing think tag (not first — vLLM partitions on first, breaking multiple
  blocks; NVIDIA "infer-missing-opener" uses last) → strip Markdown code fences → **balanced-brace
  scan returning the LAST top-level object** → `kaptinlin/jsonrepair` (pure-Go, MIT) as last resort →
  strict `Unmarshal` into `output_schema` (the validator stays the gate). *Caveats:* Qwen3 needs both
  tags; tag-less JSON falls through to the brace-scan; `json-repair` can yield a different valid JSON
  on truncation, so it runs only after strict-parse fails with schema validation still downstream.
  **Ship this.** (3) *Optional* ≤1 corrective reprompt
  (Instructor re-ask / LangChain OutputFixingParser) — strictly **adapter-internal and bounded**,
  framed as repair (never engine retry; preserves *retry ≠ repair*).
- **Reachability → free check only (P13).** Cut the paid probe (a probe-pass ≠ real-call-pass — false
  confidence; and a `Probe` method on every `Adapter` is a speculative seam the modularity rule
  forbids — CV8). Keep a thin ladder: a **free** preflight (credential presence/config validity,
  optional free `GET /models`) + the loud typed first-failure from ②.

**Invariant interaction:** none — `ir` literal scans and adapter-internal parsing are both already
sanctioned.

### ⑥ Fix the two genuine defects
*P3, P12.*

- **P3:** thread `moduleID` into `selectAgentStepSkills`→`buildSkillCorpus` and look up via
  `QualifiedAssetKey(moduleID, id)` (engine/skills.go; mirrors the already-correct
  `resolveInputFileEntries` at agent_step.go:190). Upstreams the customer's patch; root workflows
  unaffected (`moduleID==""`). **TDD: a sub-workflow skill-library conformance fixture (fake backend).**
- **P12:** stop emitting `AWF3002` on a gate evaluator's last node — mark it referenced when the
  reference pass recognizes the `{{ evaluate.<field> }}` verdict channel (validate_refs.go:75-79 vs the
  `evaluate` arm at 677-683). **TDD: a conformance fixture with an agent gate evaluator asserting no
  AWF3002.**

---

## 6. Coverage (all 14 → change)

P1→⑤ · P2→② · **P3→⑥** · P4→① · P5→① · P6→① · P7→① · P8→①+doc · P9→②+⑤ · P10→③ · P11→④ ·
**P12→⑥** · P13→②+⑤ · P14→①. Nothing dropped.

## 7. Workstream decomposition & sequencing

Each gets its own spec → plan. Ordered by leverage × independence × no-format-gate-first:

1. **WS-1 — Defects (⑥).** P3 qualified-key, P12 AWF3002. Tiny, no format change, immediate.
2. **WS-2 — Richer causes (②).** map.item `Cause`, obs enrichment, droid/goose status→cause map.
   Extends existing architecture; no format change; unblocks honest debugging of everything else.
3. **WS-3 — Docker keepalive + `cmd:` (①, the master stroke).** Default-inject + author override +
   `ir.Container.Cmd`. **Gated on a man:228-232 readiness revision.** Dissolves the native cascade.
4. **WS-4 — Honest validate (⑤ + ③'s lint).** Misplaced-key warning, deterministic schema
   tag-stripping, shell-host injection lint, free reachability check. Additive; no format change.
5. **WS-5 — Native sandbox isolation (① residual).** go-landlock (Linux) + sandbox-exec (macOS)
   restrict-then-exec (host creds readable / writes confined to scratch), optional bubblewrap,
   workdir-relative staging + loud absolute-path warning. G1 closed; sequenced after WS-3 makes native
   rare.
6. **WS-6 — Resume incrementality (④).** Small: `--from` targets the failed frontier node now.
   Per-node content addressing = a separate, **§8-gated** research spec (carries gap G2).

WS-1/WS-2/WS-4 start immediately. WS-3 and the ④ research spec each gate on a format-standard revision
first (a code change that contradicts the man page is wrong until the format is revised separately).

**TDD/conformance is the definition of done (AGENTS.md):** every workstream leads with fake-backend
conformance tests before implementation; ②, ⑥ each ship a named fixture.

## 8. Format-standard revisions required (do these first, in their workstream)

- **①** → man BACKENDS/readiness (228-232): the runtime injects a default keepalive for a command-less
  image, with an author `cmd:`/`keepalive:` override; the native backend write-confines steps via an
  OS-native sandbox (host creds readable, writes ephemeral); native records an ignored image as a
  durable deviation.
- **④ (deferred spec only)** → man §8 CHECKPOINTING AND RESUME: per-node addressing; an edit confined
  to one subtree preserves committed siblings.
- **③ needs no templating revision** — the lint is additive and consistent with the existing contract.

## 9. Gaps — closed/reduced by the methods research (2026-06-20)

- **G1 — CLOSED. Native isolation = OS-native sandbox, not a HOME-swap.** Adopt the standard
  agent-CLI sandbox: `landlock-lsm/go-landlock` (Linux, in-process Go) + `sandbox-exec`/Seatbelt
  (macOS), or bubblewrap (Linux, stronger). Read-allow keeps host creds readable; write-confine
  isolates writes to ephemeral scratch — no fake HOME to seed. Folded into ①. *Residual:*
  process-isolation is not a hardware boundary (right altitude for our threat model); macOS
  `sandbox-exec` is officially deprecated (still ships, no replacement as of macOS 26.3) — track
  Apple's stance; watch Anthropic Sandbox Runtime (`srt`) and Turso AgentFS.
- **G2 — CLOSED by scoping.** A step is not a build action; agent steps are non-hermetic (network +
  clock + host config) and cannot be content-reused. Per-node reuse is restricted to deterministic
  (code/shell) steps with declared inputs; agent steps keep exact commit/replay. Folded into ④.
- **G3 — REDUCED to the standard (brittle) classifier.** `x-should-retry` header → body `type`/`code`
  allowlist → `Retry-After` heuristic. No universal structured field exists; substring matching is
  industry-standard but brittle to wording changes. Folded into ②.

## 10. Out of scope (unchanged constraints)

Single-host only. No distributed dispatch, multi-tenancy, durable-execution guarantees, or
saga/compensation machinery (standard §12, design §16). Checkpointing skips work; it does not
distribute it. The gate's independence + crash≠verdict invariants are preserved (② strengthens them).
The interpreter remains the only writer to `state`. `with:` stays opaque to the *engine* (⑤ reads only
literal key names at validate time). Templating stays "not a language" (③ adds no rendering power).
