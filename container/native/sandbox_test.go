package native

// WHITE-BOX tests (package native — not native_test) so tests can call
// the unexported detectSandbox / credDirs directly.
//
// Scope: OS-agnostic seam + cred-dir enumerator + no-op fallback.
// Per-OS launcher selection (bwrap / sandbox-exec) is tested in T3/T4.

import (
	"errors"
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
	argv := launcher.prepend("/tmp/scratch", nil)
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
		got := launcher.prepend(c.scratch, c.roDirs)
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
