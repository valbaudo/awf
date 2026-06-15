# Native Backend Workspace Snapshots + Resume — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--backend native` runs resumable by giving the native backend a traversal-safe `Snapshot`/`Restore` over its workdir, then lifting the categorical resume refusal.

**Architecture:** The engine's snapshot/restore seam is already backend-agnostic, so no engine change. Native implements the existing `container.Backend` `Snapshot`/`Restore`: `Snapshot` walks the workdir into a deterministic gzip-tar blob; `Restore` extracts it through a single `os.Root` (TOCTOU-safe at `openat`) with three decompression-bomb limits. Resume admission flips from a hard error to mirror-Docker. A precursor commit fixes an unrelated resume-admission doc bug in the same man-page block.

**Tech Stack:** Go 1.26 (≥1.26.2 for `os.Root` CVE fix), `archive/tar`, `compress/gzip`, `os.Root` (Go 1.24/1.25), `state.Blobs` CAS, `golangci-lint` v2.

**Spec:** `docs/superpowers/specs/2026-06-15-native-resume-snapshot-design.md`

**Pre-commit verification (every commit):** `make lint test` — NOT `go test`/`go vet`/`gofmt` alone (CI runs `golangci-lint`; errcheck/staticcheck violations fail the build).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `man/awf-workflow.5.md` | precursor: fix resume-admission contract drift | 0 |
| `container/backend.go` | add `SnapshotFSArchive` enum value | 1 |
| `container/backendtest/backendtest.go` | accept the new mode in the closed Caps switch | 1 |
| `container/native/backend.go` | variadic `New`, options, blob-contingent `Capabilities` | 2 |
| `container/native/snapshot.go` (new) | header ctor, `Snapshot`, `Restore`, capped writer/reader, sentinel | 3,4,5,6 |
| `container/native/snapshot_test.go` (new) | determinism, round-trip, security, decompression tests | 3,4,5,6 |
| `ir/validate_structural.go`, `ir/diagnostic.go` | container-name charset validation | 7 |
| `cli/backend.go`, `cli/resume.go`, `cli/run.go` | resume admission flip, caveat, blob wiring | 8 |
| `cli/backend_test.go`, `cli/resume_test.go`, `cli/run_test.go`, `cli/run_backend_integ_test.go` | update broken old-contract tests | 8 |
| `cli/native_resume_integ_test.go` (new) | end-to-end run→pause→resume on native | 9 |
| `conformance/snapshot.go` | H5: assert content survival, add negative case | 10 |
| `man/awf.1.md`, `README.md`, `docs/runtime-design.md` | native-resume doc pass | 11 |

**Dependency order:** 0 (standalone) → 1 → 2 → {3, 7} → 4 → 5 → 6 → 8 → 9; 10 and 11 after 8. Tasks 3 and 7 are independent of each other; 7 is independent of all native code.

---

## Task 0: Precursor — fix resume-admission contract drift (audit hole #1)

No code; lands first so the native doc pass rebases on a correct base. Authority: `cli/resume_admission.go:11-16` admits **every** non-ok terminal outcome (and interrupted runs); only finished-`ok` is refused.

**Files:**
- Modify: `man/awf-workflow.5.md:1418-1423` and `:1445-1446`

- [ ] **Step 1: Read the authority and the man page block**

Run: `sed -n '8,42p' cli/resume_admission.go` then `sed -n '1411,1450p' man/awf-workflow.5.md`
Expected: the code admits non-ok terminal + interrupted; the man page says `ok`/`permanent_failure`/`rejected`/`cancelled` are "not resumable" (the bug).

- [ ] **Step 2: Rewrite the admission prose**

In `man/awf-workflow.5.md`, replace the two passages so they state: *a run is resumable iff its last outcome is not `ok` (`retryable_failure`, `permanent_failure`, `rejected`, or `cancelled`) or it was interrupted; a run that finished `ok` is a no-op.* Add one sentence distinguishing the `awf ls` `resumable` **label** (`obs/runstatus.go` — `retryable_failure` only) from the resume **admission** policy (every non-ok). Use the `updating-the-manual` skill for tone/format.

- [ ] **Step 3: Verify no code/test references the old wording**

Run: `grep -rn "not resumable" man/ | grep -iE "permanent|rejected|cancelled"`
Expected: no matches remain.

- [ ] **Step 4: Commit**

```bash
git add man/awf-workflow.5.md
git commit -m "docs(man): resume admits every non-ok terminal outcome (awf-workflow.5)"
```

---

## Task 1: Add the `SnapshotFSArchive` Caps value

**Files:**
- Modify: `container/backend.go:191-198` (enum), `container/backendtest/backendtest.go:84-92` (`testCapsKnownMode`)

- [ ] **Step 1: Add the enum value**

In `container/backend.go`, in the `SnapshotMode` const block:

```go
	// SnapshotFSArchive — the backend captures and restores a FULL gzip-tar
	// archive of the container workspace (no base image to diff against). The
	// native backend (host workdir). Caps is a self-description: this is NOT a
	// CoW diff, so it must not advertise SnapshotFSCoW. Behaviorally the engine
	// treats any non-SnapshotNone mode identically; this value exists to keep
	// the Caps honest, not to drive a code branch (YAGNI: do not add FSCoW-vs-
	// FSArchive switches in engine logic).
	SnapshotFSArchive SnapshotMode = "fs-archive"
```

- [ ] **Step 2: Run the backendtest contract to see the closed switch break**

Run: `go test ./container/backendtest/... ./container/native/... 2>&1 | head -20`
Expected: when a native-with-blobs backend later advertises `fs-archive`, `testCapsKnownMode` (`backendtest.go:86`) hits the `default` arm and `t.Errorf`s. (No native impl yet, so this is informational — confirm the switch is the failure site, NOT a lint error.)

- [ ] **Step 3: Add the case to the closed switch**

In `container/backendtest/backendtest.go`, `testCapsKnownMode`:

```go
	switch m := b.Capabilities().Snapshot; m {
	case container.SnapshotNone, container.SnapshotFSCoW, container.SnapshotFSArchive:
		// ok
	default:
		t.Errorf("Capabilities().Snapshot = %q, want %q, %q, or %q",
			m, container.SnapshotNone, container.SnapshotFSCoW, container.SnapshotFSArchive)
	}
```

- [ ] **Step 4: Verify build + lint**

Run: `make lint test 2>&1 | tail -20`
Expected: PASS (no consumer branches exhaustively on `SnapshotMode`; `exhaustive` linter is not enabled).

- [ ] **Step 5: Commit**

```bash
git add container/backend.go container/backendtest/backendtest.go
git commit -m "feat(container): add SnapshotFSArchive Caps value for native"
```

---

## Task 2: Native constructor options + blob-contingent Capabilities

**Files:**
- Modify: `container/native/backend.go`
- Test: `container/native/backend_test.go`

- [ ] **Step 1: Write failing tests for options + Caps**

Add to `container/native/backend_test.go`:

```go
func TestNativeCapsNoBlobsIsSnapshotNone(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Snapshot caps without blobs = %q, want %q", got, container.SnapshotNone)
	}
}

func TestNativeCapsWithBlobsIsArchive(t *testing.T) {
	b, err := native.New(t.TempDir(), native.WithBlobs(state.NewInMemoryBlobs()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotFSArchive {
		t.Errorf("Snapshot caps with blobs = %q, want %q", got, container.SnapshotFSArchive)
	}
}

func TestNativeWithSnapshotMaxBlobBytesRejectsNonPositive(t *testing.T) {
	if _, err := native.New(t.TempDir(), native.WithSnapshotMaxBlobBytes(0)); err == nil {
		t.Error("WithSnapshotMaxBlobBytes(0): err = nil, want non-nil")
	}
}
```

Add imports `"github.com/valbaudo/awf/container"` and `"github.com/valbaudo/awf/state"` to the test file if absent.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./container/native/ -run 'TestNativeCaps|TestNativeWithSnapshot' 2>&1 | head`
Expected: FAIL — `native.WithBlobs`/`native.WithSnapshotMaxBlobBytes` undefined.

- [ ] **Step 3: Add options + fields + contingent Caps**

In `container/native/backend.go`, add the `state` import, extend the struct, add the option type, rewrite `New` and `Capabilities`:

```go
// Option configures a native Backend at construction.
type Option func(*Backend) error

// WithBlobs supplies the CAS store Snapshot/Restore use. Without it the backend
// advertises SnapshotNone and Snapshot returns a descriptive error.
func WithBlobs(b state.Blobs) Option {
	return func(n *Backend) error { n.blobs = b; return nil }
}

// WithSnapshotMaxBlobBytes overrides the compressed snapshot-blob cap (default
// snapshotDefaultMaxBlobBytes). n must be > 0.
func WithSnapshotMaxBlobBytes(n int64) Option {
	return func(b *Backend) error {
		if n <= 0 {
			return fmt.Errorf("container/native: WithSnapshotMaxBlobBytes: n must be > 0, got %d", n)
		}
		b.snapshotMaxBlobBytes = n
		return nil
	}
}

// WithSnapshotMaxRestoreBytes overrides the cumulative decompressed-bytes cap
// (default snapshotDefaultMaxRestoreBytes). n must be > 0.
func WithSnapshotMaxRestoreBytes(n int64) Option {
	return func(b *Backend) error {
		if n <= 0 {
			return fmt.Errorf("container/native: WithSnapshotMaxRestoreBytes: n must be > 0, got %d", n)
		}
		b.snapshotMaxRestoreBytes = n
		return nil
	}
}
```

Add to the `Backend` struct: `blobs state.Blobs`, `snapshotMaxBlobBytes int64`, `snapshotMaxRestoreBytes int64`. Add the consts near the top of the package:

```go
const (
	snapshotDefaultMaxBlobBytes    int64 = 256 << 20 // 256 MiB compressed
	snapshotDefaultMaxRestoreBytes int64 = 4 << 30   // 4 GiB decompressed (256 MiB × 16, under DEFLATE's ~1032× max ratio)
	snapshotMaxEntries             int   = 1_000_000  // entry-count cap (inode/flat-bomb backstop)
)
```

Rewrite `New`:

```go
func New(workdirRoot string, opts ...Option) (*Backend, error) {
	if workdirRoot == "" {
		return nil, errors.New("container/native: New: workdirRoot is required")
	}
	if err := os.MkdirAll(container.AWFOutputDir, 0o755); err != nil {
		return nil, err
	}
	b := &Backend{
		workdirRoot:             workdirRoot,
		handles:                 map[string]nativeHandle{},
		snapshotMaxBlobBytes:    snapshotDefaultMaxBlobBytes,
		snapshotMaxRestoreBytes: snapshotDefaultMaxRestoreBytes,
	}
	for _, opt := range opts {
		if err := opt(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}
```

Rewrite `Capabilities`:

```go
func (b *Backend) Capabilities() container.Caps {
	snap := container.SnapshotNone
	if b.blobs != nil {
		snap = container.SnapshotFSArchive
	}
	return container.Caps{Snapshot: snap, RuntimeImage: false, RuntimeCompose: false}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./container/native/ -run 'TestNativeCaps|TestNativeWithSnapshot' -v 2>&1 | tail`
Expected: PASS. Also `go build ./...` to confirm the 38 existing `native.New(...)` callers still compile (variadic — no churn).

- [ ] **Step 5: Commit**

```bash
git add container/native/backend.go container/native/backend_test.go
git commit -m "feat(native): variadic New with WithBlobs; blob-contingent snapshot caps"
```

---

## Task 3: Deterministic tar-header constructor

**Files:**
- Create: `container/native/snapshot.go`
- Test: `container/native/snapshot_test.go`

- [ ] **Step 1: Write the determinism + leak tests**

Create `container/native/snapshot_test.go`:

```go
package native

import (
	"archive/tar"
	"io/fs"
	"os"
	"testing"
)

func TestTarHeaderZeroesOwnerAndTime(t *testing.T) {
	h := tarHeader("a.txt", tar.TypeReg, 0o644, 3, "")
	if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
		t.Errorf("owner leak: uid=%d gid=%d uname=%q gname=%q", h.Uid, h.Gid, h.Uname, h.Gname)
	}
	if !h.ModTime.IsZero() || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
		t.Errorf("time leak: mod=%v acc=%v chg=%v", h.ModTime, h.AccessTime, h.ChangeTime)
	}
}

func TestTarHeaderPreservesExecMasksSpecial(t *testing.T) {
	// exec preserved
	if got := tarHeader("x", tar.TypeReg, 0o755, 0, "").Mode; got != 0o755 {
		t.Errorf("exec mode = %o, want 0755", got)
	}
	// setuid/setgid/sticky stripped
	mode := fs.FileMode(0o755) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if got := tarHeader("x", tar.TypeReg, mode, 0, "").Mode; got != 0o755 {
		t.Errorf("special-bit mode = %o, want 0755 (special bits masked)", got)
	}
}
```

(Determinism end-to-end — identical content → identical `SnapshotRef` — is asserted in Task 4, after `Snapshot` exists.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./container/native/ -run TestTarHeader 2>&1 | head`
Expected: FAIL — `tarHeader` undefined.

- [ ] **Step 3: Implement the constructor**

Create `container/native/snapshot.go` with the package clause, imports, and:

```go
// tarHeader builds a DETERMINISTIC tar header: zero mtime/atime/ctime and zero
// owner identity (uid/gid/uname/gname), so identical workspace content yields a
// byte-identical archive (the blob = content-hash invariant). Never use
// tar.FileInfoHeader — it leaks the runner's uid/gid/uname/gname + real mtime.
// For reg/dir the mode is masked to permission bits (exec preserved;
// setuid/setgid/sticky stripped). Format is left unset so archive/tar picks the
// minimal per-entry format (USTAR for short paths, PAX only when a path is long
// or a file is large) — deterministic given zeroed time/owner, and unlike a
// forced USTAR it does not fail on long paths or files > 8 GiB.
func tarHeader(name string, typeflag byte, mode fs.FileMode, size int64, linkname string) *tar.Header {
	h := &tar.Header{Name: name, Typeflag: typeflag, Linkname: linkname}
	switch typeflag {
	case tar.TypeReg:
		h.Mode = int64(mode & os.ModePerm)
		h.Size = size
	case tar.TypeDir:
		h.Mode = int64(mode & os.ModePerm)
	case tar.TypeSymlink:
		h.Mode = 0o777 // conventional; symlink perms are ignored by the kernel
	}
	return h
}
```

Imports for this file: `"archive/tar"`, `"io/fs"`, `"os"` (plus more added in later tasks).

- [ ] **Step 4: Run tests**

Run: `go test ./container/native/ -run TestTarHeader -v 2>&1 | tail`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add container/native/snapshot.go container/native/snapshot_test.go
git commit -m "feat(native): deterministic tar-header constructor"
```

---

## Task 4: Native `Snapshot` (capture)

**Files:**
- Modify: `container/native/snapshot.go`, `container/native/snapshot_test.go`

- [ ] **Step 1: Write Snapshot tests (no-blobs error + determinism + non-empty ref)**

Add to `container/native/snapshot_test.go` (add imports `"context"`, `"errors"`, `"path/filepath"`, `"github.com/valbaudo/awf/container"`, `"github.com/valbaudo/awf/state"`):

```go
func TestSnapshotWithoutBlobsErrors(t *testing.T) {
	b, _ := New(t.TempDir())
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	if _, err := b.Snapshot(context.Background(), h); err == nil {
		t.Fatal("Snapshot without blobs: err = nil, want non-nil")
	}
}

func TestSnapshotDeterministicRef(t *testing.T) {
	mk := func() container.SnapshotRef {
		root := t.TempDir()
		b, _ := New(root, WithBlobs(state.NewInMemoryBlobs()))
		h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
		// write identical content under the per-container workdir
		wd := filepath.Join(root, "ws")
		if err := os.WriteFile(filepath.Join(wd, "a.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ref, err := b.Snapshot(context.Background(), h)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return ref
	}
	if a, b2 := mk(), mk(); a != b2 || a == "" {
		t.Errorf("non-deterministic or empty SnapshotRef: %q vs %q", a, b2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./container/native/ -run TestSnapshot 2>&1 | head`
Expected: FAIL — `Snapshot` still returns `ErrUnsupported`.

- [ ] **Step 3: Implement Snapshot + capped writer + sentinel**

In `container/native/snapshot.go`, add (imports `"bytes"`, `"compress/gzip"`, `"context"`, `"fmt"`, `"io"`, `"path/filepath"`, `"github.com/valbaudo/awf/container"`):

```go
// nativeSnapshotTooLarge reports container.ErrSnapshotTooLarge via errors.Is so
// the engine classifies a too-large snapshot as permanent_failure without
// importing this concrete type.
type nativeSnapshotTooLarge struct{ n, limit int64 }

func (e *nativeSnapshotTooLarge) Error() string {
	return fmt.Sprintf("container/native: snapshot exceeds size limit: %d > %d bytes", e.n, e.limit)
}
func (e *nativeSnapshotTooLarge) Is(target error) bool { return target == container.ErrSnapshotTooLarge }

// cappedWriter trips *nativeSnapshotTooLarge once cumulative bytes exceed limit.
type cappedWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, &nativeSnapshotTooLarge{n: c.n + int64(len(p)), limit: c.limit}
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Snapshot captures the container workdir as a deterministic gzip-tar blob.
// Enforces TWO caps: the compressed-output cap (cappedWriter on the gzip stream)
// and the decompressed-total cap (uncompressed bytes fed in) — the latter
// symmetric with Restore so an unrestorable snapshot cannot be created.
func (b *Backend) Snapshot(ctx context.Context, h container.Handle) (container.SnapshotRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("container/native: Snapshot: unknown handle %q", h.ID)
	}
	if b.blobs == nil {
		return "", fmt.Errorf("container/native: Snapshot: no blob store (construct with native.WithBlobs)")
	}

	var buf bytes.Buffer
	cw := &cappedWriter{w: &buf, limit: b.snapshotMaxBlobBytes}
	gw := gzip.NewWriter(cw) // bare header: no Name, no mtime, OS byte 255
	tw := tar.NewWriter(gw)

	var uncompressed int64
	entries := 0
	walkErr := filepath.WalkDir(r.workdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == r.workdir { // skip the root entry
			return nil
		}
		rel, err := filepath.Rel(r.workdir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entries++
		if entries > snapshotMaxEntries {
			return &nativeSnapshotTooLarge{n: int64(entries), limit: int64(snapshotMaxEntries)}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(tarHeader(rel+"/", tar.TypeDir, info.Mode(), 0, ""))
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p) // WalkDir does NOT follow symlinks; captured verbatim
			if err != nil {
				return err
			}
			return tw.WriteHeader(tarHeader(rel, tar.TypeSymlink, info.Mode(), 0, target))
		case info.Mode().IsRegular():
			uncompressed += info.Size()
			if uncompressed > b.snapshotMaxRestoreBytes {
				return &nativeSnapshotTooLarge{n: uncompressed, limit: b.snapshotMaxRestoreBytes}
			}
			if err := tw.WriteHeader(tarHeader(rel, tar.TypeReg, info.Mode(), info.Size(), "")); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, cpErr := io.Copy(tw, f)
			_ = f.Close()
			return cpErr
		default:
			return nil // fifo/socket/device skipped
		}
	})
	if walkErr != nil {
		return "", fmt.Errorf("container/native: Snapshot: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	ref, err := b.blobs.Put(buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("container/native: Snapshot: blobs.Put: %w", err)
	}
	return container.SnapshotRef(ref), nil
}
```

Delete the old `Snapshot` stub in `container/native/backend.go` (the one returning `ErrUnsupported`).

- [ ] **Step 4: Run tests**

Run: `go test ./container/native/ -run TestSnapshot -v 2>&1 | tail`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add container/native/snapshot.go container/native/snapshot_test.go container/native/backend.go
git commit -m "feat(native): Snapshot captures workdir as deterministic gzip-tar"
```

---

## Task 5: Native `Restore` (`os.Root`-confined) + round-trip + security

**Files:**
- Modify: `container/native/snapshot.go`, `container/native/snapshot_test.go`

- [ ] **Step 1: Write round-trip + security tests**

Add to `container/native/snapshot_test.go`:

```go
func TestRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	b, _ := New(root, WithBlobs(blobs))
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	wd := filepath.Join(root, "ws")
	_ = os.WriteFile(filepath.Join(wd, "a.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wd, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	ref, err := b.Snapshot(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Destroy(context.Background(), h)

	h2, err := b.Restore(context.Background(), ref, "ws")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "ws", "a.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Errorf("restored a.txt = %q, %v", got, err)
	}
	fi, _ := os.Stat(filepath.Join(root, "ws", "run.sh"))
	if fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("exec bit not preserved on restore: %v", fi)
	}
	_ = h2
}

func TestRestoreRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	b, _ := New(root, WithBlobs(state.NewInMemoryBlobs()))
	// plant a victim OUTSIDE root that must survive
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "keep")
	_ = os.WriteFile(victim, []byte("x"), 0o644)
	for _, name := range []string{"", "..", "../escape", "/abs", "a/../../b", "."} {
		if _, err := b.Restore(context.Background(), container.SnapshotRef("awf-d1:sha256:"+"00"), name); err == nil {
			t.Errorf("Restore(name=%q): err = nil, want non-nil", name)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim outside root was disturbed: %v", err)
	}
}

func TestRestoreSymlinkTraversalConfined(t *testing.T) {
	// A blob containing `evil -> <victimDir>` then a regular `evil/x` must NOT
	// write through the symlink: os.Root refuses to traverse it. Build the blob
	// by hand so the malicious entries are exact.
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	victimDir := t.TempDir()
	ref := buildMaliciousSymlinkBlob(t, blobs, victimDir) // helper below
	b, _ := New(root, WithBlobs(blobs))
	_, err := b.Restore(context.Background(), ref, "ws")
	if err == nil {
		t.Fatal("Restore of write-through-symlink blob: err = nil, want escape error")
	}
	if _, statErr := os.Stat(filepath.Join(victimDir, "x")); statErr == nil {
		t.Fatal("write-through symlink escaped os.Root: victim/x was created")
	}
}
```

Add a helper in the test file that builds a gzip-tar with a `tar.TypeSymlink` entry `evil` → `victimDir` followed by a `tar.TypeReg` entry `evil/x`, gzips it, and `blobs.Put`s it, returning the `container.SnapshotRef`. (Use `archive/tar` + `compress/gzip` directly; ~20 lines.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./container/native/ -run TestRestore 2>&1 | head`
Expected: FAIL — `Restore` still returns `ErrUnsupported`.

- [ ] **Step 3: Implement Restore via `os.Root`**

In `container/native/snapshot.go` add (no decompression cap yet — Task 6):

```go
// Restore re-materializes a container workdir from a SnapshotRef. EVERY
// filesystem op goes through one os.Root rooted at the (trusted, fixed) workdir,
// so a '..' name or an attacker-planted/captured symlink cannot redirect a write
// outside the workdir (TOCTOU-safe at openat; the same primitive loader.go uses).
// Perms are set AT CREATE (no post-create Root.Chmod) to avoid CVE-2026-32282.
func (b *Backend) Restore(ctx context.Context, ref container.SnapshotRef, name string) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}
	if name == "" {
		return container.Handle{}, fmt.Errorf("container/native: Restore: name is required")
	}
	if b.blobs == nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: no blob store (construct with native.WithBlobs)")
	}
	if !filepath.IsLocal(name) { // rejects "", "..", "/abs", "a/../../b"; defense-in-depth before OpenRoot
		return container.Handle{}, fmt.Errorf("container/native: Restore: unsafe container name %q", name)
	}
	workdir := filepath.Join(b.workdirRoot, name)

	rootDir, err := os.OpenRoot(b.workdirRoot)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: open root: %w", err)
	}
	defer func() { _ = rootDir.Close() }()

	if err := rootDir.RemoveAll(name); err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: remove %q: %w", name, err)
	}
	if err := rootDir.MkdirAll(name, 0o755); err != nil {
		return container.Handle{}, fmt.Errorf("container/native: Restore: mkdir %q: %w", name, err)
	}

	if err := b.extractInto(rootDir, name, ref); err != nil {
		_ = os.RemoveAll(workdir) // single cleanup path
		return container.Handle{}, err
	}

	b.mu.Lock()
	b.handles[workdir] = nativeHandle{workdir: workdir}
	b.mu.Unlock()
	return container.Handle{Name: name, ID: workdir}, nil
}

// extractInto streams the blob's gzip-tar into <root>/<base> via the root.
// Task 6 wraps the decompressor in the three decompression caps.
func (b *Backend) extractInto(root *os.Root, base string, ref container.SnapshotRef) error {
	blob, err := b.blobs.Get(string(ref))
	if err != nil {
		return fmt.Errorf("container/native: Restore: blobs.Get: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("container/native: Restore: gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("container/native: Restore: tar: %w", err)
		}
		rel := path.Join(base, path.Clean("/"+hdr.Name)) // join under base; leading-slash-stripped
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, fs.FileMode(hdr.Mode)&os.ModePerm); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("container/native: Restore: mkdir %q: %w", rel, err)
			}
		case tar.TypeReg:
			if err := ensureParent(root, rel); err != nil {
				return err
			}
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return fmt.Errorf("container/native: Restore: create %q: %w", rel, err)
			}
			_, cpErr := io.Copy(f, tr) // Task 6 replaces tr with a capped reader + io.CopyN
			_ = f.Close()
			if cpErr != nil {
				return fmt.Errorf("container/native: Restore: write %q: %w", rel, cpErr)
			}
		case tar.TypeSymlink:
			if err := ensureParent(root, rel); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, rel); err != nil { // target verbatim (decision 7)
				return fmt.Errorf("container/native: Restore: symlink %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("container/native: Restore: unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// ensureParent creates rel's parent directories through the root (confined).
func ensureParent(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("container/native: Restore: mkdir parent %q: %w", dir, err)
	}
	return nil
}
```

Add imports `"errors"` and `"path"`. Delete the old `Restore` stub in `container/native/backend.go`.

> **Implementer note (verify, per rule 4):** confirm `(*os.Root).MkdirAll`, `RemoveAll`, `Symlink`, `OpenFile`, `Mkdir` exist on the pinned toolchain (`go doc os.Root`; they landed in Go 1.24/1.25). If `MkdirAll` is absent, walk components with `root.Mkdir` ignoring `fs.ErrExist`.

- [ ] **Step 4: Run tests**

Run: `go test ./container/native/ -run TestRestore -v 2>&1 | tail`
Expected: PASS — including the symlink-traversal test asserting the escape error and an untouched victim. If the escape surfaces as `*os.PathError "path escapes from parent"`, the test's "err != nil" + "victim absent" assertions cover it (do NOT assert `fs.ErrPermission`).

- [ ] **Step 5: Add the shared backendtest Caps + double-destroy coverage**

Run: `go test ./container/native/ -run 'TestNative|TestRestore|TestSnapshot|TestTarHeader' 2>&1 | tail`
Expected: PASS. (Native is exercised by `RunBasicContract` elsewhere; `RunSnapshotContract` stays Docker-only.)

- [ ] **Step 6: Commit**

```bash
git add container/native/snapshot.go container/native/snapshot_test.go container/native/backend.go
git commit -m "feat(native): os.Root-confined Restore (traversal-safe extraction)"
```

---

## Task 6: Restore decompression-bomb limits

**Files:**
- Modify: `container/native/snapshot.go`, `container/native/snapshot_test.go`

- [ ] **Step 1: Write the three bomb tests**

Add to `container/native/snapshot_test.go` helpers that build raw gzip-tar blobs, plus:

```go
func TestRestoreTripsCumulativeByteCap(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	// 3 files each 0.4 MiB; cap 1 MiB → cumulative (1.2 MiB) trips even though
	// no single file exceeds the cap.
	ref := buildBlobWithFiles(t, blobs, []int{400 << 10, 400 << 10, 400 << 10})
	b, _ := New(root, WithBlobs(blobs), WithSnapshotMaxRestoreBytes(1<<20))
	_, err := b.Restore(context.Background(), ref, "ws")
	if !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ws")); statErr == nil {
		t.Error("partial workdir not cleaned up after cap trip")
	}
}

func TestRestoreTripsEntryCountCap(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	ref := buildBlobWithNZeroLenEntries(t, blobs, 10) // helper; test overrides cap to 5
	b, _ := New(root, WithBlobs(blobs))
	b.maxEntries = 5 // test hook (see Step 3)
	_, err := b.Restore(context.Background(), ref, "ws")
	if !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
}

func TestRestoreIgnoresLyingSizeHeader(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	ref := buildBlobWithLyingSize(t, blobs, "big", 10<<30, []byte("short")) // hdr.Size=10GiB, body 5 bytes
	b, _ := New(root, WithBlobs(blobs), WithSnapshotMaxRestoreBytes(64<<20))
	if _, err := b.Restore(context.Background(), ref, "ws"); err != nil {
		t.Fatalf("short body behind a lying Size header must restore: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "ws", "big"))
	if string(got) != "short" {
		t.Errorf("restored %q, want \"short\"", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./container/native/ -run 'TestRestoreTrips|TestRestoreIgnoresLying' 2>&1 | head`
Expected: FAIL (no cap yet; lying-size copies 10 GiB or the cumulative/entry caps don't exist).

- [ ] **Step 3: Add the capped reader + entry cap + per-file CopyN**

In `container/native/snapshot.go`, add a cumulative-decompressed capped reader and rewire `extractInto`. Add a `maxEntries int` field to `Backend` defaulting to `snapshotMaxEntries` in `New` (so tests can override):

```go
// cappedReader bounds cumulative decompressed bytes across the WHOLE restore
// (counts bytes READ from the decompressor — never trusts tar hdr.Size).
type cappedReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, &nativeSnapshotTooLarge{n: c.n, limit: c.limit}
	}
	return n, err
}
```

In `extractInto`, wrap the gzip reader before the tar reader and enforce the entry cap + per-file bound:

```go
	capped := &cappedReader{r: gr, limit: b.snapshotMaxRestoreBytes}
	tr := tar.NewReader(capped)
	entries := 0
	for {
		hdr, err := tr.Next()
		// ... EOF/err handling unchanged ...
		entries++
		if entries > b.maxEntries {
			return &nativeSnapshotTooLarge{n: int64(entries), limit: int64(b.maxEntries)}
		}
		// ... dir/symlink unchanged; for TypeReg replace io.Copy(f, tr) with: ...
		if _, cpErr := io.CopyN(f, tr, b.snapshotMaxRestoreBytes+1); cpErr != nil && cpErr != io.EOF {
			_ = f.Close()
			return fmt.Errorf("container/native: Restore: write %q: %w", rel, cpErr)
		}
		_ = f.Close()
```

(The `cappedReader` is the load-bearing cumulative budget; the per-file `io.CopyN(..., max+1)` is defense-in-depth and reading past EOF returns `io.EOF` which is not an error here.)

Set `b.maxEntries = snapshotMaxEntries` in `New` (after defaults).

- [ ] **Step 4: Run tests**

Run: `go test ./container/native/ -run 'TestRestore|TestSnapshot' -v 2>&1 | tail`
Expected: PASS — cumulative trip, entry-count trip, lying-size restores the short body, workdir cleaned on trip.

- [ ] **Step 5: Document the docker asymmetry**

Add a one-line comment at `container/docker/snapshot.go:432` (above `streamPlainTarFromDiff`): docker Restore is intentionally unbounded because `CopyToContainer` extracts into the isolated container fs (container-disk DoS only, not the host) — recorded decision, no code change.

- [ ] **Step 6: Commit**

```bash
git add container/native/snapshot.go container/native/snapshot_test.go container/docker/snapshot.go
git commit -m "feat(native): three decompression-bomb limits on Restore"
```

---

## Task 7: IR container-name validation (C2 layer 1)

**Files:**
- Modify: `ir/validate_structural.go`, `ir/diagnostic.go`
- Test: `ir/validate_structural_test.go`

- [ ] **Step 1: Golden-compat check (gate before locking the regex)**

Run: `grep -rEh "^\s{2,}[A-Za-z0-9_./:-]+:\s*$" examples 2>/dev/null; grep -rn "containers:" -A6 examples | grep -E "^\S" | head -40`
Then inspect every container key under `containers:` in `examples/**` and any golden workflow. Expected: all keys match `^[A-Za-z_][A-Za-z0-9_-]*$`. If a legit key uses `.` or starts with a digit, widen the regex deliberately and note it here before proceeding.

- [ ] **Step 2: Write the failing validation test**

Add to `ir/validate_structural_test.go` (follow the `envNamePattern` convention at `:737` — assert the message contains the regex string):

```go
func TestValidateRejectsBadContainerName(t *testing.T) {
	ld := loadInlineWorkflow(t, `
id: t
version: "1"
containers:
  "../escape":
    image: "x@sha256:`+strings.Repeat("a", 64)+`"
steps:
  - id: s
    container: "../escape"
    run: "true"
`)
	diags := ir.Validate(ld)
	msg := firstDiagMessage(t, diags, "AWF1026")
	if !strings.Contains(msg, containerNamePatternString) {
		t.Errorf("AWF1026 message %q must contain the enforcing regex", msg)
	}
}
```

(Use the test file's existing inline-load + diagnostic-lookup helpers; `containerNamePatternString` = `containerNamePattern.String()` exported via an `export_test.go` alias if the pattern is unexported, matching the `envNamePattern` test access.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./ir/ -run TestValidateRejectsBadContainerName 2>&1 | head`
Expected: FAIL — no AWF1026, name currently accepted.

- [ ] **Step 4: Add the pattern + diagnostic**

In `ir/validate_structural.go`, near `envNamePattern` (`:105`):

```go
// containerNamePattern constrains container map keys to a path-safe identifier
// charset (mirrors stepIDPattern; bars '/', '\', '..', ':', '.'). The native
// backend derives a host workdir from this name (filepath.Join + RemoveAll on
// resume), so an unconstrained key is a path-traversal sink; docker sanitizes
// via containerName() but native does not. The validator sees RAW per-workflow
// keys ('::' qualification is composed at engine runtime), so the charset is strict.
var containerNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
```

In the `for name, ctr := range wf.Containers` loop (`:41`), add at the top:

```go
		if !containerNamePattern.MatchString(name) {
			c.errf(ContainerPath(name, ""), "AWF1026", fmt.Sprintf("%s: %q (must match %s)", catalog["AWF1026"], name, containerNamePattern))
		}
```

In `ir/diagnostic.go`, add: `"AWF1026": "container name uses an unsupported charset (must be a path-safe identifier)",`

- [ ] **Step 5: Run tests + full IR suite (regression for goldens)**

Run: `go test ./ir/... 2>&1 | tail`
Expected: PASS, including all existing golden/validate tests (confirms Step 1's compat check).

- [ ] **Step 6: Commit**

```bash
git add ir/validate_structural.go ir/diagnostic.go ir/validate_structural_test.go
git commit -m "feat(ir): validate container-name charset (AWF1026, path-traversal guard)"
```

---

## Task 8: CLI wiring — resume admission flip, caveat, blob wiring

**Files:**
- Modify: `cli/backend.go`, `cli/resume.go`, `cli/run.go`
- Test: `cli/backend_test.go`, `cli/resume_test.go`, `cli/run_test.go`, `cli/run_backend_integ_test.go`

- [ ] **Step 1: Flip the admission test (TDD red) — rename + invert**

In `cli/backend_test.go`, change the native-rejection test (`:142`, asserts `"not resumable"`) into `TestReadBackendKindFromLogAdmitsNative`:

```go
func TestReadBackendKindFromLogAdmitsNative(t *testing.T) {
	events := []state.Event{runStartedEventWithBackend(t, engine.BackendNative)}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil (native is now resumable)", err)
	}
	if kind != engine.BackendNative {
		t.Errorf("kind = %q, want %q", kind, engine.BackendNative)
	}
}
```

Run: `go test ./cli/ -run TestReadBackendKindFromLogAdmitsNative 2>&1 | head`
Expected: FAIL — current arm returns the "not resumable" error.

- [ ] **Step 2: Flip the admission arm**

In `cli/backend.go`, `readBackendKindFromLog`, change the `case engine.BackendNative` arm from the `fmt.Errorf("... not resumable ...")` to:

```go
		case engine.BackendNative:
			return kind, nil
```

Run: `go test ./cli/ -run TestReadBackendKindFromLogAdmitsNative 2>&1 | tail` → PASS.

- [ ] **Step 3: Wire blobs into the native backend + nil-blobs panic**

In `cli/backend.go`, `newBackend`, native arm:

```go
	case engine.BackendNative:
		if blobs == nil {
			panic("cli: newBackend native: blobs must not be nil") // OpenBlobs never returns nil-without-error; callers exit first
		}
		b, err := native.New(workdirRoot, native.WithBlobs(blobs))
		if err != nil {
			return nil, nil, fmt.Errorf("cli: construct native backend: %w", err)
		}
		return b, func() {}, nil
```

- [ ] **Step 4: Add the resume-time caveat**

In `cli/resume.go`, after `readBackendKindFromLog` returns `kind` (around `:190`):

```go
	if kind == engine.BackendNative {
		fprintf(stderr, "awf resume: native backend — committed work is replayed and snapshot: workspace workdirs are restored, but the host base environment is not pinned; shell-step tooling runs against the current host.\n")
	}
```

- [ ] **Step 5: Reword the auto-native run note + update its tests**

In `cli/run.go:339`, replace the "cannot be resumed until native resume is supported" message with:

```go
		fprintf(stderr, "awf run: auto-selected native backend (no Docker-only features). Resume restores snapshot: workspace workdirs but does not pin the host base environment; use --backend docker for a pinned baseline.\n")
```

Update the two literal-string asserts in `cli/run_test.go` (`:1194`, `:1217`) to the new `wantWarning` text. Update `cli/resume_test.go:353` to build a *genuinely resumable* native fixture (matching digest + resolved runtime, else it would error at `resume.go:249`/`:377` for the wrong reason) and assert `rc != ExitUsage` plus the §7 caveat substring. Re-check `cli/run_backend_integ_test.go:449/451` and adjust to the new admission/strings.

- [ ] **Step 6: Run the CLI suite**

Run: `make lint test 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cli/backend.go cli/resume.go cli/run.go cli/backend_test.go cli/resume_test.go cli/run_test.go cli/run_backend_integ_test.go
git commit -m "feat(cli): native resume admitted; host-env caveat; blob wiring"
```

---

## Task 9: Native end-to-end run→pause→resume integ test

**Files:**
- Create: `cli/native_resume_integ_test.go`

- [ ] **Step 1: Write the e2e test (await/pause idiom)**

Clone the structure of `TestCLIRunDockerBackendPauseResumeRoundTrip` (find it: `grep -rn "PauseResumeRoundTrip" cli/`). Workflow: step 1 in a `snapshot: workspace` container writes a sentinel file and commits, then an `await` (or pause signal) suspends; step 2 reads the sentinel. After pause, `os.RemoveAll` the container workdir so Restore is the **sole** source of the sentinel; then `awf resume`; assert `run.finished{ok}`, step-2 `node.completed`, `SnapshotRef != ""` via a re-fold of the log, and the sentinel content is present for step 2. Run with `--backend native` (no Docker, no API).

```go
//go:build integ
// +build integ

package cli_test
// ... build a temp state dir + workflow file; run `awf run --backend native ... --run-id r1` to a pause;
// os.RemoveAll(<stateDir>/work/r1/<container>); run `awf resume r1 <wf>`; assert ok + sentinel restored.
```

- [ ] **Step 2: Run it**

Run: `go test -tags integ ./cli/ -run TestNativeResume 2>&1 | tail`
Expected: PASS — proves snapshot **content** survives a real native run→resume (the thing the fake conformance does not assert).

- [ ] **Step 3: Commit**

```bash
git add cli/native_resume_integ_test.go
git commit -m "test(cli): native run->pause->resume restores workspace snapshot (e2e)"
```

---

## Task 10: Fake conformance — assert content survival (H5)

**Files:**
- Modify: `conformance/snapshot.go`

- [ ] **Step 1: Read the existing bucket**

Run: `sed -n '1,80p' conformance/snapshot.go` (find `testSnapshotRestoreCalledOnResume`).
Expected: it asserts `RestoreCalls` contains `{Name, Ref}` (dispatch), not content.

- [ ] **Step 2: Strengthen it + add a negative case**

Add to the snapshot conformance: step 1 uses `fake.ProgramFiles` to write a file into the snapshot:workspace container; after resume, a step `CaptureFiles` the restored handle and the suite asserts the content matches. Add a sibling test where a container WITHOUT `snapshot: workspace` is `Created` fresh on resume (assert no `RestoreCalls` for it).

```go
// after resume, assert restored content (not just the Restore dispatch)
// and a negative: non-snapshot container takes the Create path (no RestoreCalls).
```

- [ ] **Step 3: Run the conformance suite**

Run: `go test ./conformance/... 2>&1 | tail`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add conformance/snapshot.go
git commit -m "test(conformance): assert snapshot content survives resume (not just dispatch)"
```

---

## Task 11: Native-resume documentation pass

**Files:**
- Modify: `man/awf.1.md`, `man/awf-workflow.5.md`, `README.md`, `docs/runtime-design.md`

Use the `updating-the-manual` skill for the man pages.

- [ ] **Step 1: `man/awf.1.md`**

Rewrite the native-not-resumable claims: `--backend` (`:149`), resume (`:195-196`, `:205-210`), and the exact auto-native string (`:154`) to match the new `cli/run.go:339` message byte-for-byte. State: native IS resumable; `snapshot: workspace` workdirs are restored; host base-env is not pinned.

- [ ] **Step 2: `man/awf-workflow.5.md:213-217`** — same correction (native resumable; workspace via snapshot; host base-env caveat).

- [ ] **Step 3: `README.md:219`** — change the native bullet: drop "not resumable" → "resumable; workspace via `snapshot: workspace`; host base-env not pinned."

- [ ] **Step 4: `docs/runtime-design.md`** — update `:231` (run/resume narrative), `:351` (native Caps note: snapshot now `fs-archive`), `:490-491` (CLI table resume row).

- [ ] **Step 5: Verify no stale claims remain**

Run: `grep -rn -i "native.*not resumable\|not resumable.*native" man/ README.md docs/`
Expected: no matches.

- [ ] **Step 6: Commit**

```bash
git add man/awf.1.md man/awf-workflow.5.md README.md docs/runtime-design.md
git commit -m "docs: native backend is resumable (workspace via snapshot; host base-env caveat)"
```

---

## Self-review (completed)

**Spec coverage:** §2 decisions 1–11 each map to a task — admission (8), `SnapshotFSArchive` (1), contingent caps (2), `os.Root` Restore (5), symlink verbatim (5), three caps (6), determinism (3,4), name validation (7), self-contained tar (3–6). §10 determinism → 3/4; §11 caps → 6; §8 name → 7; §14 docs incl. hole-#1 precursor → 0 + 11; §13 tests distributed; §16 risks encoded as implementer notes (os.Root API verify in T5, CVE-avoided-by-create-perms in T5, regex golden-compat gate in T7). No gap found.

**Placeholder scan:** no "TBD"/"add error handling"/"similar to Task N"; each code step shows real code. Two explicit *verify* notes (os.Root method set; regex golden-compat) are flagged uncertainties per project rule 4, not placeholders — each has a concrete fallback.

**Type consistency:** `tarHeader`, `cappedWriter`, `cappedReader`, `nativeSnapshotTooLarge`, `extractInto`, `ensureParent`, `WithBlobs`, `WithSnapshotMaxBlobBytes`, `WithSnapshotMaxRestoreBytes`, `containerNamePattern`, `AWF1026`, `b.maxEntries` are defined once and referenced consistently. `native.New(workdirRoot, opts...)` signature is stable across tasks 2–9.
