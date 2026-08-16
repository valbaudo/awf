package native

// WHITE-BOX tests (package native — not native_test) so tests can call
// the unexported detectSandbox / credDirs directly.
//
// Scope: OS-agnostic seam + cred-dir enumerator + no-op fallback.
// Per-OS launcher selection (bwrap / sandbox-exec) is tested in T3/T4.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
)

// errNotFound is a stand-in for exec.ErrNotFound in mock lookPath calls.
var errNotFound = errors.New("not found")

// mockNotFound returns a lookPath func that always reports "not found".
func mockNotFound() func(string) (string, error) {
	return func(_ string) (string, error) {
		return "", errNotFound
	}
}

// TestDetectSandbox_NoOpFallback asserts that when detectPlatformSandbox
// returns (nil, "") — true on every platform with the current stubs —
// detectSandbox falls back to the no-op launcher and returns a non-empty
// warn label.
func TestDetectSandbox_NoOpFallback(t *testing.T) {
	launcher, label := detectSandbox(mockNotFound())

	if launcher == nil {
		t.Fatal("detectSandbox: launcher = nil, want non-nil no-op launcher")
	}
	// The no-op launcher must return nil argv (no prefix prepended).
	argv := launcher.prepend("/tmp/scratch", nil, nil)
	if argv != nil {
		t.Errorf("no-op launcher.prepend() = %v, want nil", argv)
	}
	// Label must be non-empty.
	if label == "" {
		t.Error("detectSandbox no-op fallback: label = \"\", want non-empty warn label")
	}
}

// TestDetectSandbox_NoOpPrepend confirms the no-op launcher's prepend always
// returns nil regardless of arguments.
func TestDetectSandbox_NoOpPrepend(t *testing.T) {
	launcher, _ := detectSandbox(mockNotFound())

	cases := []struct {
		scratch string
		roDirs  []string
	}{
		{"/tmp/x", nil},
		{"/tmp/x", []string{"/home/user/.claude"}},
		{"", nil},
	}
	for _, c := range cases {
		got := launcher.prepend(c.scratch, nil, c.roDirs)
		if got != nil {
			t.Errorf("noOp.prepend(%q, %v) = %v, want nil", c.scratch, c.roDirs, got)
		}
	}
}

// TestCredDirs_HomeAndDefaults asserts that credDirs returns the standard
// agent config dirs when HOME is set and CODEX_HOME is unset.
func TestCredDirs_HomeAndDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "") // clear to exercise ~/.codex fallback

	dirs := credDirs(home)

	wants := []string{
		home + "/.claude",
		home + "/.codex", // CODEX_HOME unset → ~/.codex
		home + "/.factory",
		home + "/.config/goose",
		home + "/.config",
	}
	for _, w := range wants {
		found := false
		for _, d := range dirs {
			if d == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("credDirs: missing %q; got %v", w, dirs)
		}
	}
}

// TestCredDirs_CodexHomeEnv asserts that $CODEX_HOME overrides ~/.codex.
func TestCredDirs_CodexHomeEnv(t *testing.T) {
	home := t.TempDir()
	customCodex := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", customCodex)

	dirs := credDirs(home)

	// customCodex must appear.
	found := false
	for _, d := range dirs {
		if d == customCodex {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("credDirs with CODEX_HOME=%q: custom path not in %v", customCodex, dirs)
	}
	// Default ~/.codex must NOT appear when CODEX_HOME is set.
	defaultCodex := home + "/.codex"
	for _, d := range dirs {
		if d == defaultCodex {
			t.Errorf("credDirs with CODEX_HOME set: default %q still appears in %v", defaultCodex, dirs)
		}
	}
}

// TestCredDirs_NoDuplicates asserts credDirs produces no duplicate entries
// even when env vars overlap with default paths.
func TestCredDirs_NoDuplicates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home+"/.config") // deliberately overlaps with ~/.config

	dirs := credDirs(home)
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Errorf("credDirs: duplicate entry %q in %v", d, dirs)
		}
		seen[d] = true
	}
}

// TestWithSandbox_Construction verifies that WithSandbox(true/false) can be
// passed to New without error.
func TestWithSandbox_Construction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := New(dir, WithSandbox(true)); err != nil {
		t.Fatalf("New with WithSandbox(true): %v", err)
	}
	if _, err := New(dir, WithSandbox(false)); err != nil {
		t.Fatalf("New with WithSandbox(false): %v", err)
	}
}

// TestWithSandbox_CapabilitiesUnchanged verifies that WithSandbox does not
// alter the existing Capabilities() contract (snapshot, image, compose).
func TestWithSandbox_CapabilitiesUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := New(dir, WithSandbox(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := b.Capabilities()
	if caps.Snapshot != container.SnapshotNone {
		t.Errorf("Capabilities().Snapshot = %q, want %q", caps.Snapshot, container.SnapshotNone)
	}
	if caps.RuntimeImage {
		t.Error("Capabilities().RuntimeImage = true, want false")
	}
	if caps.RuntimeCompose {
		t.Error("Capabilities().RuntimeCompose = true, want false")
	}
}

// TestNative_SandboxModeExposed_Disabled asserts that SandboxMode() reports
// "none" when sandboxing was never requested — both the explicit
// WithSandbox(false) and the omitted-option default. detectSandbox is never
// invoked on this path, so the assertion is deterministic on every platform
// (F30: the label detectSandbox resolves was previously discarded at
// backend.go's `l, _ := detectSandbox(exec.LookPath)`).
func TestNative_SandboxModeExposed_Disabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b, err := New(dir, WithSandbox(false))
	if err != nil {
		t.Fatalf("New with WithSandbox(false): %v", err)
	}
	if got := b.SandboxMode(); got != "none" {
		t.Errorf("SandboxMode() with WithSandbox(false) = %q, want %q", got, "none")
	}

	b2, err := New(dir)
	if err != nil {
		t.Fatalf("New with no WithSandbox option: %v", err)
	}
	if got := b2.SandboxMode(); got != "none" {
		t.Errorf("SandboxMode() with no WithSandbox option = %q, want %q", got, "none")
	}
}

// TestNative_SandboxModeExposed_ToolNotFound asserts that when sandbox mode
// is requested but the injected lookPath stub reports the platform tool
// absent, SandboxMode() reports "none" rather than propagating the long
// human-readable noSandboxWarnLabel text (that text stays reserved for
// SandboxWarnLabel — a distinct concern from this concise status token).
//
// "landlock-trampoline" is also accepted here: on a Landlock-capable Linux
// host, detectPlatformSandbox probes the kernel directly (not lookPath), so
// defeating the bwrap lookup alone does not force the no-op fallback there
// (see TestSandboxLauncherLabel_WarnSubstring for the same caveat).
func TestNative_SandboxModeExposed_ToolNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b, err := New(dir, withSandboxLookPath(true, mockNotFound()))
	if err != nil {
		t.Fatalf("New with withSandboxLookPath(true, mockNotFound()): %v", err)
	}
	switch got := b.SandboxMode(); got {
	case "none", "landlock-trampoline":
		// ok
	default:
		t.Errorf("SandboxMode() = %q, want \"none\" (or \"landlock-trampoline\" on a Landlock-capable Linux host)", got)
	}
}

// TestSandboxLauncherLabel_WarnSubstring checks that the no-op fallback label
// loudly signals the absence of OS-level isolation.
//
// It asserts on the noSandboxWarnLabel constant directly rather than driving
// detectSandbox(mockNotFound()): mockNotFound only defeats PATH lookups, but on
// Linux detectPlatformSandbox also probes the kernel for Landlock (a syscall,
// not a PATH binary), so a sandbox-capable host returns the real
// "landlock-trampoline" launcher and never reaches the no-op fallback. The
// loud-label invariant lives on the constant that fallback returns.
func TestSandboxLauncherLabel_WarnSubstring(t *testing.T) {
	lower := strings.ToLower(noSandboxWarnLabel)
	if !strings.Contains(lower, "no") && !strings.Contains(lower, "warn") {
		t.Errorf("noSandboxWarnLabel %q: expected \"no\" or \"warn\" substring", noSandboxWarnLabel)
	}
}

// hasDir reports whether want appears in dirs.
func hasDir(dirs []string, want string) bool {
	for _, d := range dirs {
		if d == want {
			return true
		}
	}
	return false
}

// TestCredDirsWritable_ExcludesConfigCatchAll is the load-bearing security
// assertion for T2: the per-agent config dirs are writable (so a token refresh
// persists across runs — verified on Linux: droid -> ~/.factory/auth.v2.file,
// codex -> ~/.codex/auth.json) but the bare ~/.config catch-all is NOT, so a
// step never gains write to the whole XDG tree (git, gh, shell). ~/.config
// stays READABLE via credDirs.
func TestCredDirsWritable_ExcludesConfigCatchAll(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	rw := credDirsWritable(home)
	for _, w := range []string{
		home + "/.claude",
		home + "/.codex",
		home + "/.factory",
		home + "/.config/goose",
	} {
		if !hasDir(rw, w) {
			t.Errorf("credDirsWritable missing agent dir %q; got %v", w, rw)
		}
	}

	bare := home + "/.config"
	if hasDir(rw, bare) {
		t.Errorf("credDirsWritable includes the bare XDG catch-all %q (would grant write to git/gh/shell config); got %v", bare, rw)
	}
	if !hasDir(credDirs(home), bare) {
		t.Errorf("credDirs (read-only baseline) dropped %q; the catch-all must stay readable; got %v", bare, credDirs(home))
	}
}

// TestCredDirsWritable_CodexHomeEnv asserts $CODEX_HOME overrides ~/.codex in
// the writable set too (parity with credDirs).
func TestCredDirsWritable_CodexHomeEnv(t *testing.T) {
	home := t.TempDir()
	customCodex := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", customCodex)

	rw := credDirsWritable(home)
	if !hasDir(rw, customCodex) {
		t.Errorf("credDirsWritable with CODEX_HOME=%q: not in %v", customCodex, rw)
	}
	if hasDir(rw, home+"/.codex") {
		t.Errorf("credDirsWritable: default ~/.codex present despite CODEX_HOME set; got %v", rw)
	}
}

// TestToolDirs_UserPrefix asserts ~/.local is granted read-only (read+execute)
// so an agent CLI installed under the user prefix stays runnable inside the
// sandbox instead of dying with an opaque shell exit 126. The whole ~/.local
// subtree is granted, not just ~/.local/bin, because a bin entry can symlink
// into ~/.local/lib (verified on Linux: ~/.local/bin/claude ->
// ~/.local/lib/node_modules/.../claude.exe).
func TestToolDirs_UserPrefix(t *testing.T) {
	home := t.TempDir()

	got := toolDirs(home)
	want := home + "/.local"
	if !hasDir(got, want) {
		t.Errorf("toolDirs missing %q; got %v", want, got)
	}
	// Exec needs read+execute only — never write.
	if hasDir(credDirsWritable(home), want) {
		t.Errorf("toolDirs entry %q must not be writable; credDirsWritable=%v", want, credDirsWritable(home))
	}
}

// REGRESSION (adversarial review F1): $CODEX_HOME is env-derived, so a value of
// $HOME (or any ancestor) would put the whole home dir in the WRITABLE set. The
// launcher re-binds writable dirs on top of its `--tmpfs $HOME`, so that would
// restore the entire real home read-write inside the sandbox — ~/.ssh included.
func TestCredDirsWritable_RefusesHomeAndAncestors(t *testing.T) {
	home := t.TempDir()
	for _, broad := range []string{home, filepath.Dir(home), "/"} {
		t.Setenv("HOME", home)
		t.Setenv("CODEX_HOME", broad)

		rw := credDirsWritable(home)
		if hasDir(rw, broad) {
			t.Errorf("CODEX_HOME=%q: writable set contains it (%v) — would undo the HOME tmpfs", broad, rw)
		}
		// The narrow, legitimate dirs must still be granted.
		if !hasDir(rw, home+"/.factory") {
			t.Errorf("CODEX_HOME=%q: legitimate ~/.factory dropped; got %v", broad, rw)
		}
	}
}

// REGRESSION (adversarial review F2/F4): the writable credential dirs and the
// ~/.local tool prefix are agent-runtime grants. A code (`run:`) step must get
// neither — it has no token to refresh and no agent CLI to locate, so granting
// them would let a shell step overwrite another agent's credentials and read
// ~/.local/share.
func TestSandboxDirsFor_ScopedToAgentRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	codeRW, codeRO := sandboxDirsFor(container.Cmd{Run: "echo hi"}, home)
	if len(codeRW) != 0 {
		t.Errorf("code step got writable dirs %v, want none", codeRW)
	}
	if hasDir(codeRO, home+"/.local") {
		t.Errorf("code step got the ~/.local tool prefix; roDirs=%v", codeRO)
	}
	// The pre-existing read-only credential baseline is unchanged for code steps.
	if !hasDir(codeRO, home+"/.claude") {
		t.Errorf("code step lost the read-only credential baseline; roDirs=%v", codeRO)
	}

	agentRW, agentRO := sandboxDirsFor(container.Cmd{Run: "claude -p x", AgentRuntime: true}, home)
	if !hasDir(agentRW, home+"/.factory") {
		t.Errorf("agent exec missing writable ~/.factory; rwDirs=%v", agentRW)
	}
	if !hasDir(agentRO, home+"/.local") {
		t.Errorf("agent exec missing the ~/.local tool prefix; roDirs=%v", agentRO)
	}
	if hasDir(agentRW, home+"/.config") {
		t.Errorf("agent exec must not get the bare ~/.config catch-all writable; rwDirs=%v", agentRW)
	}
}

// Run 9486dda3 (2026-08-16): on a fresh runner, /root/.codex does not exist,
// and the landlock RW grant for it is silently skipped (IgnoreIfMissing —
// correct for optional host dirs like /lib64, wrong for dirs we INTEND to
// create). The codex login prelude's mkdir -p then ran INSIDE the sandbox and
// died EACCES. The sandbox must pre-create the dirs it means to grant.
func TestEnsureWritableDirs(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, ".codex")
	if err := ensureWritableDirs([]string{nested}); err != nil {
		t.Fatalf("ensureWritableDirs: %v", err)
	}
	info, err := os.Stat(nested)
	if err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// idempotent on an existing dir
	if err := ensureWritableDirs([]string{nested}); err != nil {
		t.Fatalf("ensureWritableDirs (existing): %v", err)
	}
	// a FILE in the way is an error, never a silent skip
	filePath := filepath.Join(root, "blocked")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureWritableDirs([]string{filePath}); err == nil {
		t.Fatal("a file at the target must be an error")
	}
}

// Run fabac8fa (2026-08-16): codex under the landlock trampoline died with an
// opaque "error sending request" — the actual failure was DNS: on
// systemd-resolved hosts /etc/resolv.conf is a symlink into
// /run/systemd/resolve/, OUTSIDE every granted dir, and landlock follows the
// symlink → resolver config unreadable → no nameservers. The RO grant set must
// include the symlink's resolved target (as a FILE grant — landlock rejects
// directory access rights on regular files).
func TestResolverExtraROFiles(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	run := filepath.Join(root, "run", "systemd", "resolve")
	if err := os.MkdirAll(filepath.Join(etc, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(run, "stub-resolv.conf")
	if err := os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// symlink escaping the granted tree → the resolved target is granted
	link := filepath.Join(etc, "resolv.conf")
	if err := os.Symlink(filepath.Join("..", "run", "systemd", "resolve", "stub-resolv.conf"), link); err != nil {
		t.Fatal(err)
	}
	got := resolverExtraROFiles(link, etc)
	wantTarget, _ := filepath.EvalSymlinks(target) // macOS: /var → /private/var
	if len(got) != 1 || got[0] != wantTarget {
		t.Fatalf("escaping symlink: got %v, want [%s]", got, wantTarget)
	}

	// plain file (non-systemd host) → nothing extra
	plain := filepath.Join(etc, "resolv-conf-plain")
	if err := os.WriteFile(plain, []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolverExtraROFiles(plain, etc); got != nil {
		t.Fatalf("plain file: got %v, want nil", got)
	}

	// symlink staying INSIDE the granted tree → already covered, nothing extra
	if err := os.Symlink("inner.conf", filepath.Join(etc, "resolv-inner")); err != nil {
		t.Fatal(err)
	}
	if got := resolverExtraROFiles(filepath.Join(etc, "resolv-inner"), etc); got != nil {
		t.Fatalf("internal symlink: got %v, want nil", got)
	}

	// missing file → nothing (IgnoreIfMissing-style tolerance)
	if got := resolverExtraROFiles(filepath.Join(root, "nope"), etc); got != nil {
		t.Fatalf("missing: got %v, want nil", got)
	}
}
