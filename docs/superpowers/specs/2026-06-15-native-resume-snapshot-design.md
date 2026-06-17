# Native backend: workspace snapshots + resume

**Date:** 2026-06-15
**Status:** design, pending implementation
**Toolchain:** Go 1.26 (≥1.26.2 required — see §16 CVE-2026-32282).
**Scope:** make `--backend native` runs resumable by giving the native backend a real,
traversal-safe `Snapshot`/`Restore` over its workdir, then lifting the categorical resume refusal.

> Revision note (2026-06-15): this spec was rewritten after a code-grounded + prior-art review.
> The security sections (Restore extraction, decompression caps, tar determinism) carry the
> *post-review* solutions — `os.Root` confinement, three independent decompression limits, a
> deterministic tar-header constructor — not the weaker first-draft guards.

## 1. Goal & motivation

Today `awf resume` of a native run hard-errors categorically in
`cli/backend.go:readBackendKindFromLog` (the `engine.BackendNative` arm). The refusal is
**conservative policy, not a capability gap**: the resume flow downstream of that arm is
backend-agnostic (verified — `engine/local_dispatcher.go:294` fires `Snapshot` on
`intent.ResolvedInputs.Snapshot=="workspace"`, *not* on `Caps==FSCoW`; then
`engine/commit.go:125` → `engine/events.go:399` → `engine/fold.go:227` →
`cli/resume.go:347`), and native's `Create` is a trivial `mkdir`. What native lacks is a way to
capture/restore workspace state, so a resumed frontier would see an empty workdir with no
signal that state was lost.

This change adds native workspace snapshots, then allows native resume on the **mirror-Docker**
policy:

- `snapshot: workspace` containers are `Restore`d from their committed blob.
- containers without it are `Create`d fresh (empty workdir) — exactly as at run start.
- resume is always admitted (the run already proved it has no Docker-only features); the
  snapshot is opt-in fidelity, not an admission gate.

### The caveat native does NOT close

Native runs on the host with no isolation and **no base-image pin**. A snapshot captures the
container **workdir** (`<workdirRoot>/<name>/`), not host-wide state (system packages, files
written outside the workdir, `/tmp/awf`) and not the host base environment. So native resume
preserves **checkpoint integrity** (committed work replayed; definition-digest pin; agent-
runtime-drift pin — all unchanged) but **not** baseline reproducibility for shell-step tooling.
This is identical to native's existing "out-of-spec for digest-pinned reproducibility" stance
(`container/native/backend.go:1-17`); resume inherits it, it does not worsen it. `awf resume`
prints a one-line caveat to stderr on native runs.

## 2. Locked decisions

1. **Mirror-Docker admission** (user-approved). Native resume always admitted; snapshot is
   opt-in fidelity. Non-snapshot (and no-ref) containers `Create` fresh (`resume.go:347` else-arm).
2. **Honest Caps value: `container.SnapshotFSArchive` (`"fs-archive"`).** Native captures a full
   workdir archive, not a CoW diff; `"fs-cow"` would be a false self-description. Reuse is *not*
   cheaper — the gating cost is the closed allow-list switch + Caps-asserting tests, not the const.
3. **Blob-contingent capability** (mirrors the fake, `container/fake.go:184`). `native.New` gains
   variadic options; `Capabilities().Snapshot` returns `SnapshotFSArchive` iff blobs were supplied,
   else `SnapshotNone`. The 38 existing `native.New(...)` callers compile unchanged. **`Snapshot`
   with nil blobs returns a descriptive `WithBlobs`-naming error (not bare `ErrUnsupported`); the
   production `newBackend` native arm panics on nil blobs** (it always has them — `OpenBlobs`
   never returns nil-without-error and callers exit on its error first).
4. **Full archive, self-contained per snapshot.** No base image ⇒ no diff; each snapshot is a
   complete gzip-tar of the workdir.
5. **Bare-CAS `SnapshotRef`.** Native's `SnapshotRef` is the plain blob ref (`awf-d1:sha256:…`) —
   no `@image@cmd` segments. `SnapshotRef` is opaque to the engine and backend-private; the
   backend kind is pinned in `run.started`, so only native ever parses a native ref.
6. **Traversal-safe Restore via `os.Root`** (§5). Every Restore filesystem op — `RemoveAll`,
   `MkdirAll`, file/dir/symlink creation — goes through one `os.Root` rooted at the workdir. This
   is the same primitive the repo already uses (`loader/loader.go:38`, `cli/live_home.go`),
   empirically TOCTOU-safe at the `openat` layer on go1.26.4. Lexical path checks
   (`filepath.Clean`/`Base`/`IsLocal`) are necessary-but-insufficient and are NOT the mechanism.
7. **Symlink-target policy: store verbatim + document (option b).** `Root.Symlink` does not
   validate targets; native captures/restores symlinks faithfully (including escaping/absolute
   targets). Rationale: native is *explicitly* no-isolation — the post-restore frontier shell
   steps already touch the whole host (`backend.go:14-17`), so a restored symlink grants nothing
   new; `os.Root` still prevents the *extraction itself* from being redirected outside the workdir.
   The residual (a persisted escaping symlink the frontier may follow) is documented as native's
   existing reality; **option (a) — reject/normalize escaping/absolute targets at snapshot time —
   is recorded as a future hardening toggle**, not implemented now.
8. **Three independent decompression limits on Restore** (§11), symmetric at Snapshot so an
   unrestorable snapshot can't be created: cumulative decompressed-byte budget (counted at the
   decompressor, never trusting `hdr.Size`), entry-count cap, and a decompressed-total ceiling
   distinct from the 256 MiB *compressed* cap. All wired to `container.ErrSnapshotTooLarge`.
9. **Deterministic tar headers** (§10): a dedicated header constructor, never `tar.FileInfoHeader`
   (which leaks host uid/gid/uname/gname + mtime). Preserve exec bit, mask setuid/setgid/sticky;
   zero all time + owner fields. A determinism test (identical content → identical `SnapshotRef`)
   is the guarantee.
10. **Container-name validation at the IR** (§8, C2 layer 1): defense-in-depth + format hygiene,
    behind a golden-compat gate; `os.Root` (decision 6) is the load-bearing runtime guard.
11. **Native self-contained tar code.** Docker's `diffTarWriter`/`cappedWriter` are unexported in
    package `docker`; native writes its own in `container/native/snapshot.go` (extracting a shared
    helper would touch working docker code — out of scope).
12. **Option A — explicit `--backend native` runs image-mode + `snapshot: workspace` on the host.**
    The run-time backend *selector* (`selectRunBackendForLoadedDefinition` /
    `firstNativeIncompatibleFeature` in `cli/backend_features.go`) was relaxed: static image-mode
    and `snapshot: workspace` no longer abort an explicit native run — they run on the host,
    ignoring the declared image (a no-isolation warning prints). Native still rejects only
    compose / runtime-compose / runtime-map-image (no host equivalent). `--backend auto` is
    unchanged: it still routes image-mode / `snapshot: workspace` / compose / runtime-* to docker
    for a pinned baseline. (Without this relax, the §6 `snapshotguard` gate alone would never be
    reached for native image-mode runs — the selector aborted first.)

## 3. Architecture — what changes, what does not

**No engine change.** Native implements the existing `container.Backend` `Snapshot`/`Restore`
(currently `ErrUnsupported`); the engine drives them through the same seam docker uses.

| Layer | Change |
|---|---|
| `container/backend.go` | add `SnapshotFSArchive` enum value (+ YAGNI-guarding doc comment) |
| `container/native/backend.go` | `New(workdirRoot string, opts ...Option)` variadic; `WithBlobs`, `WithSnapshotMaxBlobBytes` (reject n≤0); store `blobs`, caps; `Capabilities()` blob-contingent |
| `container/native/snapshot.go` (new) | `Snapshot` (walk → deterministic gzip-tar → Put), `Restore` (`os.Root`-confined untar with 3 caps), capped writer, deterministic header ctor |
| `container/backendtest/backendtest.go` | add `case SnapshotFSArchive` to `testCapsKnownMode` (`:86`) — closed switch, else `t.Errorf` |
| `ir/validate_structural.go` | add `containerNamePattern` check in the `wf.Containers` loop (`:41`) |
| `ir/diagnostic.go` | new `AWF10xx` for malformed container name |
| `cli/backend.go` | `newBackend` native arm passes blobs (+ nil-blobs panic); `readBackendKindFromLog` native arm returns `kind, nil` |
| `cli/resume.go` | print host-base-env caveat when `kind == native` |
| `cli/run.go` | reword the auto-native note (`:339`) — native IS resumable now |
| man pages, README, design doc | rewrite native-not-resumable claims; **fold audit hole #1 as a precursor commit** |

## 4. Native `Snapshot` (capture)

1. Look up the handle's workdir; unknown handle ⇒ error. `b.blobs == nil` ⇒ descriptive
   `WithBlobs` error (defensive; Caps already gates this).
2. `filepath.WalkDir(workdir)` lexical order. **Skip the root entry** (`if path == workdir`).
   For each entry, path stored **workdir-relative** (`filepath.Rel` + `filepath.ToSlash`; dir
   names get a trailing `/`):
   - dir → deterministic dir header (mode)
   - symlink → deterministic symlink header (linkname verbatim, per decision 7)
   - regular file → deterministic regular header (mode) + streamed body
   - fifo/socket/device → skipped (matches docker skipping non-regular)
3. Stream entries through `gzip → cappedWriter(compressed limit) → bytes.Buffer`, while a second
   counter enforces the **decompressed-total** cap on bytes *fed in* (decision 8, symmetric with
   Restore so a snapshot that couldn't be restored fails here, loud and early). Either cap ⇒
   `*nativeSnapshotTooLarge` whose `Is` reports `container.ErrSnapshotTooLarge`.
4. `b.blobs.Put(buf.Bytes())` → blobRef. Return `SnapshotRef(blobRef)`.

Headers use the deterministic constructor (§10), never `tar.FileInfoHeader`.

## 5. Native `Restore` (`os.Root`-confined)

1. `ctx.Err()` check; **explicit `if name == ""` error** (do not rely on `Base("")=="."`).
2. `workdir = filepath.Join(b.workdirRoot, name)`; `os.OpenRoot(b.workdirRoot)` (trusted, fixed).
   All subsequent ops go through this `*os.Root` on **relative** names.
3. `root.RemoveAll(name)` then `root.MkdirAll(name, 0o755)` — fresh baseline, confined: a `..` or
   pre-planted symlink leaf cannot escape (`RemoveAll` on a symlink-leaf unlinks the link, does
   not recurse through it — verified go1.26.4).
4. `b.blobs.Get(ref)` → gzip-tar bytes. Build the decompressor pipeline with the three caps
   (decision 8 / §11): wrap the gzip reader in a cumulative-byte counter; bound each `tar.Next()`
   against the entry-count cap; copy each body with `io.CopyN(dst, tr, perFileMax+1)` (the `+1`
   distinguishes at-cap from over-cap; bound the *copy*, never trust `hdr.Size`).
5. Per entry, via the root on the relative name:
   - dir → `root.Mkdir(name, perm)` (perm **at create** — no post-create `Chmod`, avoids
     CVE-2026-32282; §16)
   - regular → `root.OpenFile(name, O_CREATE|O_EXCL|O_WRONLY, perm)` + size-bounded copy
   - symlink → `root.Symlink(linkname, name)` (verbatim, decision 7)
   - fifo/socket/device → reject (defense-in-depth; should never appear, snapshot skips them)
   A name that escapes the root surfaces `*os.PathError` "path escapes from parent" — wrap and
   return; **never** assert `fs.ErrPermission`/`fs.ErrInvalid` in tests (verified: not those
   sentinels).
6. Register `b.handles[workdir] = nativeHandle{workdir}`; return `Handle{Name: name, ID: workdir}`.
7. On any mid-flight failure (escape, cap trip, I/O, `ENOSPC`): one `os.RemoveAll(workdir)` cleanup
   and a wrapped error (engine resume re-calls Restore from scratch — docker `restoreFail`
   precedent). Do not add a second cleanup path.

A decompression-cap trip on resume **fails hard** (clear message; not retry, not framed as infra)
— it is a poisoned/oversized artifact, distinct from the dispatcher's `snapshotFailureOutcome`
path (`Restore` is called from `cli/resume.go:347`, not the dispatcher).

## 6. Capabilities (blob-contingent)

```go
func (b *Backend) Capabilities() container.Caps {
    snap := container.SnapshotNone
    if b.blobs != nil {
        snap = container.SnapshotFSArchive
    }
    return container.Caps{Snapshot: snap, RuntimeImage: false, RuntimeCompose: false}
}
```

`RuntimeImage`/`RuntimeCompose` stay `false` (unchanged). `snapshot: workspace` on native passes
**two** gates, not one. First the run-time backend *selector*
(`cli/backend_features.go`, decision 12) must admit it — relaxed so an explicit `--backend native`
runs image-mode + `snapshot: workspace` on the host instead of aborting (the earlier selector
blocked this, so the original claim that snapshotguard alone admits it was incomplete). Then
`cli/snapshotguard.go` keys on `!= SnapshotNone`, so `snapshot: workspace` passes on
native-with-blobs and is still rejected on native-without-blobs. Production never sees nil blobs
(decision 3).

## 7. Resume admission + warnings

- `cli/backend.go readBackendKindFromLog`: `case engine.BackendNative` returns `kind, nil`
  (was: hard error). `backendAuto` and `default` arms unchanged.
- `cli/resume.go`: after `readBackendKindFromLog`, if `kind == engine.BackendNative`, print to
  stderr: `awf resume: native backend — committed work is replayed and snapshot: workspace
  workdirs are restored, but the host base environment is not pinned; shell-step tooling runs
  against the current host.`
- `cli/run.go:339`: reword the auto-native note to reflect that native IS resumable (workspace
  via snapshot; host base-env not pinned). The exact string is mirrored verbatim in
  `man/awf.1.md:154` and asserted by tests (§13) — keep all three byte-identical.
- Pins untouched: digest mismatch (`resume.go:249`) and runtime-drift (`resume.go:377`) remain
  hard errors on native exactly as on docker.

## 8. Container-name validation (C2 layer 1)

The IR validator currently does **not** charset-check container map keys (verified: only
`envNamePattern`/`stepIDPattern` exist; `container/docker/naming.go:26-29` documents the hole;
docker is immune via `containerName()` sanitize + daemon isolation). The validator sees **raw,
per-workflow** keys — `::` qualification is composed at engine runtime (`engine/path.go:121`),
never at validation — so the pattern is strict:

```go
var containerNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`) // mirrors stepIDPattern; bars '/', '\', '..', ':', '.'
```

Checked in the `for name, ctr := range wf.Containers` loop (`validate_structural.go:41`) with a
new `AWF10xx`. **Golden-compat gate:** before locking, grep every `examples/**` workflow + golden
for a container key outside this charset; if any legit name breaks (e.g. uses `.`), widen the
charset deliberately and re-pin. This layer closes the format hole for *all* backends +
`validate`/`run`; the `os.Root` of §5 is the load-bearing runtime guard regardless.

## 9. What is captured vs not (authoritative caveat list)

- **Captured:** every regular file (mode), directory (mode), and symlink (target verbatim) under
  the container's workdir at the commit boundary.
- **NOT captured:** host state outside the workdir; the host base environment (system packages,
  binaries, libraries a shell step invokes); `/tmp/awf`; anything written via absolute host paths.
- **Symlink residual (decision 7):** a captured symlink with an escaping/absolute target is
  restored verbatim; `os.Root` prevents *extraction* from writing through it, but the unisolated
  post-restore frontier may follow it — consistent with native's documented no-isolation design.
- **Consequence:** a native workflow that flows data through typed outputs / artifact references
  (the AWF model) resumes faithfully; one relying on ambient host state outside its workdir may
  drift — the same caveat that already applies to running native twice.

## 10. Determinism (H1)

`tar.FileInfoHeader` leaks the runner's identity (empirically: `Uid=501 Gid=20 Uname=vabbb
Gname=staff`) + a non-zero `ModTime`, which would break the `blob = content hash` invariant
(dedup, reproducibility). Native MUST use a dedicated header constructor:

- `Mode: int64(info.Mode() & os.ModePerm)` — **preserve exec bit, mask setuid/setgid/sticky**.
- `Uid/Gid: 0`, `Uname/Gname: ""`.
- `ModTime: time.Time{}` (zero); never set `AccessTime`/`ChangeTime`.
- `Format`: leave unset so Go selects the minimal per-entry format (USTAR for short paths, PAX
  only for long names / large files) — deterministic given zeroed time/owner fields, and unlike a
  forced USTAR it does not fail on long paths or files > 8 GiB.
- gzip: bare `gzip.NewWriter` (no `Name`; OS byte 255) — carries no mtime.

**Determinism test** (the real guarantee): tar the same workdir content twice → identical
`SnapshotRef`. Plus: exec-bit-preserved, setuid-stripped, header `Uid==0`.

## 11. Decompression-bomb defense (H2)

The existing cap is **compressed-only** (`snapshotDefaultMaxBlobBytes = 256<<20`, on gzip output
at snapshot). Docker `Restore` decompresses unbounded (`snapshot.go:432` — `gzip.NewReader` +
`io.Copy`, no `LimitReader`); docker is shielded only by the daemon extracting into the isolated
container fs. Native extracts onto the operator's host disk → attacker-influenced host
disk/inode exhaustion on resume. Native Restore enforces three independent limits (prior art: AWS
CodeGuru, SEI CERT IDS04-J, Microsoft ZIP/TAR best-practices):

1. **Cumulative decompressed-byte budget** at the decompressor (count bytes *read*, one counter
   across all entries — not per-file, which a 10k×200 MiB archive defeats). Default
   `snapshotDefaultMaxRestoreBytes = 4<<30` (256 MiB × 16, under DEFLATE's ~1032× max ratio);
   override `WithSnapshotMaxRestoreBytes` (reject n≤0).
2. **Per-file bound** via `io.CopyN(dst, tr, perFileMax+1)` — bounds the copy, ignores `hdr.Size`.
3. **Entry-count cap** (default 1,000,000) — kills the flat many-zero-length-files inode bomb the
   byte budget misses.

All trip `container/native`'s too-large sentinel (`Is` → `container.ErrSnapshotTooLarge`); resume
fails hard (§5 step 7 cleanup). Snapshot enforces the same decompressed-total ceiling (decision
8) so an unrestorable snapshot can't be created. Record a one-line comment at docker
`snapshot.go:432` that docker-restore-unbounded is **by design** (daemon isolation) — a documented
asymmetry, not an oversight.

## 12. Files / signature changes

`native.New(workdirRoot string, opts ...Option)` — variadic; `WithBlobs(state.Blobs)`,
`WithSnapshotMaxBlobBytes(int64)`, `WithSnapshotMaxRestoreBytes(int64)`. The 38 existing
`native.New(t.TempDir())` callers compile unchanged. (Chosen over a docker-style required `blobs`
param: zero caller churn + "no blob store ⇒ cannot snapshot" is the correct contingency.)

## 13. Testing

TDD red-first. AGENTS.md: durability behavior needs fake-backend conformance; native-specific
filesystem behavior is unit/integ tested on the host (no Docker, no API).

**Native snapshot/restore unit tests** (`container/native/snapshot_test.go`, pure host fs):
- round-trip (write → Snapshot → Destroy → Restore → CaptureFiles matches); fresh-workdir-on-
  restore (pre-existing junk removed); deleted-file-absent-after-restore.
- **Determinism** (§10): identical content → identical `SnapshotRef`; exec-bit preserved;
  setuid stripped; header `Uid==0`.
- **Security (C1/C2)**: (a) escaping-symlink-final-component + a file under it → error, victim
  outside untouched; (b) symlinked-parent-then-child-write (the regression a clean-join passes
  but `os.Root` rejects); name `..`, `../escape`, `/abs`, `a/../../b`, `.`, `""` each error,
  parent sentinel untouched; pre-planted `<workdirRoot>/evil -> /tmp/x` not followed by Restore;
  assert the substring `"path escapes from parent"` or victim-absent, **never** `fs.ErrPermission`.
  Legit in-root symlink round-trips.
- **Decompression (H2)**: cap-trip (one big file, low `WithSnapshotMaxRestoreBytes`); flat bomb
  (>entry-count zero-length entries); cumulative-across-files (3 files each 0.4× cap → trips);
  `hdr.Size`-lie (declared 10 GB, short body → byte budget governs, short body restores); each
  asserts `errors.Is(err, container.ErrSnapshotTooLarge)` + workdir gone + disk back to baseline.
- `Snapshot` with nil blobs → descriptive `WithBlobs` error; `WithBlobs` → Caps `SnapshotFSArchive`;
  no-blobs → `SnapshotNone`.

**`container/backendtest`**: add `case SnapshotFSArchive` at `:86`. Do NOT route native through
`RunSnapshotContract` (container-absolute `/work/...` fixtures are meaningless on the host).

**Tests to update (TDD red first) — all four+ breaking sites enumerated:**
- `cli/backend_test.go:142` — rename `RejectsNative` → `AdmitsNative`: assert `(BackendNative, nil)`.
- `cli/resume_test.go:353` — rebuild with a *genuinely-resumable* native fixture (matching digest +
  runtime, else it passes for the wrong reason at `resume.go:249`); assert `rc != ExitUsage` + the
  §7 caveat substring.
- `cli/run_test.go:1194` + `:1217` — update `wantWarning` to the reworded auto-native string (§7).
- `cli/run_backend_integ_test.go:449/451` — verify the old-contract assertions still hold / update.

**Native end-to-end resume** (alongside `container/native/*_integ_test.go` / a CLI integ test):
clone the await/pause idiom (`TestCLIRunDockerBackendPauseResumeRoundTrip`) — step 1 is
`snapshot: workspace`, writes a sentinel file, commits; pause; **`os.RemoveAll` the workdir before
resume** so Restore is the *sole* source of the sentinel; resume → assert `run.finished{ok}`,
step-2 `node.completed`, `SnapshotRef != ""` via re-fold, sentinel content present. (Avoids the
first-draft's retrying-exit-nonzero + shared-workdir non-discriminating assertion.)

**Fake conformance (H5)**: the existing `conformance/snapshot.go` bucket asserts restore is
*dispatched*, not that content *survives* — fix the §13 claim wording accordingly. Optionally
strengthen: `ProgramFiles` on step 1 + a step-2 content assertion, and a negative case (container
*without* `snapshot: workspace` → `Created` fresh, no `RestoreCalls`).

**IR (C2 L1)**: `containers: { "../x": {...} }` fails validation; assert the message contains
`containerNamePattern.String()` (matching the `envNamePattern` test convention,
`validate_structural_test.go:737`).

## 14. Documentation

Use the `updating-the-manual` skill for the man pages.

**Precursor commit (audit hole #1, zero code dependency — land first, rebase native on top):**
reconcile `man/awf-workflow.5.md:1418-1423` **and** `:1445-1446` (both wrongly say
`permanent_failure`/`rejected`/`cancelled` are "not resumable") TO the authority
`cli/resume_admission.go:11-16` — a run is resumable iff its terminal outcome is non-ok (or it was
interrupted); only finished-`ok` refuses. Add one sentence distinguishing the `awf ls`
`RunResumable` label (`obs/runstatus.go` — retryable_failure only) from the resume *admission*
policy (every non-ok). No new code test.

**Native-resume doc pass:** rewrite every native-not-resumable claim — `man/awf.1.md` `--backend`
(`:149`), resume (`:195-196`, `:205-210`), and the exact auto-native string (`:154`);
`man/awf-workflow.5.md:213-217`; `README.md:219` (drop "not resumable" → "resumable; workspace
via `snapshot: workspace`; host base-env not pinned"); `docs/runtime-design.md` (`:231`,
`:351`, `:490-491`).

## 15. Out of scope

- Host base-env pinning / outside-workdir capture / incremental or diff snapshots.
- Symlink-target rejection/normalization at snapshot (option a) — recorded future toggle (§2.7).
- Concurrency beyond the existing per-run flock (`cli/run.go:323`); the `/tmp/awf/<step>.json`
  cross-run clobber caveat (`backend.go:9-12`) is unchanged (single-run resume is flock-serialized).
- Streaming `Blobs.Put` (`state.Blobs.Put` is `[]byte`-only, `blobs.go:40`) — peak RAM ≈ the
  compressed blob (≤ cap) per snapshot commit, and on Restore the full blob + transient decompress
  buffers; honest, bounded, accepted.
- The other 7 audit findings (separate doc-hygiene sweep).
- Docker restore stays unbounded by design (daemon isolation; §11) — one comment, no code change.

## 16. Risks / verify-during-implementation

- **CVE-2026-32282** (`Root.Chmod` TOCTOU symlink escape, fixed Go 1.26.2/1.25.9): avoided by
  setting perms **at create** (`OpenFile`/`Mkdir` perm), so no post-create `Root.Chmod`. Record a
  load-bearing **minimum toolchain Go ≥ 1.26.2** invariant; CI is on go1.26.4.
- **Go issue #75335**: an absolute-target symlink that resolves *inside* the root still errors on
  read-back under `os.Root`. With store-verbatim (decision 7) the frontier reads via raw host fs
  (not `os.Root`), so frontier fidelity is unaffected; only AWF's own `os.Root` reads would hit
  this. Note it; do not have AWF re-read restored symlinks through `os.Root`.
- **Error sentinel**: the `os.Root` escape error is `*os.PathError` "path escapes from parent",
  NOT `fs.ErrPermission`/`fs.ErrInvalid` — tests must assert the substring or victim-absent.
- **Container-name regex golden-compat** (§8): verify no existing example/golden uses an
  out-of-charset container key before locking; widen deliberately if so.
- **`SnapshotFSArchive` is a TEST break, not a lint break** (corrected from the first draft):
  `golangci-lint` default set has no `exhaustive`; the breakage is the closed allow-list switch at
  `backendtest.go:86` (default `t.Errorf`). Verify with `make lint test`.

## 17. Verified non-issues (recorded so they aren't re-litigated)

- **Engine snapshot trigger is backend-agnostic** — `local_dispatcher.go:294` fires on the IR
  string `snapshot=="workspace"`, not on `Caps==FSCoW`. No engine change needed.
- **Single resume-admission gate** — only `cli/backend.go:109` + `cli/run.go:339` special-case
  native resumability; `obs/runstatus.go` keys the `awf ls` label on *outcome*, not backend (so
  this change also removes a latent ls-says-resumable/resume-refuses inconsistency for native).
- **Clean back-compat** — pre-change, `snapshot: workspace` + native was *rejected* at run start
  (native advertised `SnapshotNone`), so no existing native run can carry an orphaned
  `SnapshotRef`; old native runs resume with `Create`-fresh workdirs (no migration hazard). Tied
  to deleting/renaming `TestReadBackendKindFromLogRejectsNative`.
