//go:build darwin

package native

// WHITE-BOX darwin sandbox arg-construction tests (//go:build darwin).
// No sandbox-exec binary or actual sandboxing needed — these test argv
// assembly and SBPL profile generation only (pure function of inputs).
//
// Live isolation tests are in sandbox_darwin_integ_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realPath returns the symlink-resolved absolute path of p, mirroring the
// EvalSymlinks logic in sandboxExecLauncher.prepend (needed on macOS where
// /var and /tmp are symlinks to /private/var and /private/tmp).
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// TestSandboxExecLauncher_ArgvStructure asserts the sandbox-exec launcher
// builds the correct argv per the brief:
//
//	["sandbox-exec", "-D", "SCRATCH=<abs>", "-D", "TMPDIR=<tmpdir>",
//	 "-f", "<profile.sb>", "--", "sh", "-c", <run>]
func TestSandboxExecLauncher_ArgvStructure(t *testing.T) {
	scratch := t.TempDir()
	const run = "echo hello"

	f := &sandboxExecFactory{}
	l := f.buildForRun(run).(sandboxExecLauncher)
	argv := l.prepend(scratch, nil)

	if len(argv) < 11 {
		t.Fatalf("argv too short (%d elements): %v", len(argv), argv)
	}

	// argv[0] must be "sandbox-exec".
	if argv[0] != "sandbox-exec" {
		t.Errorf("argv[0] = %q, want \"sandbox-exec\"", argv[0])
	}
	// argv[1] == "-D", argv[2] == "SCRATCH=<abs>"
	if argv[1] != "-D" {
		t.Errorf("argv[1] = %q, want \"-D\"", argv[1])
	}
	// prepend uses EvalSymlinks to get the kernel-level real path (on macOS
	// /var is a symlink to /private/var; sandbox-exec checks real paths).
	wantScratch := "SCRATCH=" + realPath(scratch)
	if argv[2] != wantScratch {
		t.Errorf("argv[2] = %q, want %q", argv[2], wantScratch)
	}
	// argv[3] == "-D", argv[4] == "TMPDIR=<tmpdir>"
	if argv[3] != "-D" {
		t.Errorf("argv[3] = %q, want \"-D\"", argv[3])
	}
	if !strings.HasPrefix(argv[4], "TMPDIR=") {
		t.Errorf("argv[4] = %q, want TMPDIR=...", argv[4])
	}
	// argv[5] == "-f", argv[6] == <profile path>
	if argv[5] != "-f" {
		t.Errorf("argv[5] = %q, want \"-f\"", argv[5])
	}
	profilePath := argv[6]
	if !strings.HasSuffix(profilePath, ".sb") {
		t.Errorf("profile path %q: want *.sb suffix", profilePath)
	}
	// Profile file must exist and contain required SBPL lines.
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile file %q not readable: %v", profilePath, err)
	}
	profile := string(data)
	for _, want := range []string{
		"(deny file-write*)",
		`(allow file-write* (subpath (param "SCRATCH")))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q; got:\n%s", want, profile)
		}
	}
	// argv[7] == "--", argv[8] == "sh", argv[9] == "-c", argv[10] == run
	if argv[7] != "--" {
		t.Errorf("argv[7] = %q, want \"--\"", argv[7])
	}
	if argv[8] != "sh" || argv[9] != "-c" || argv[10] != run {
		t.Errorf("argv[7:11] = %v, want [-- sh -c %q]", argv[7:], run)
	}
}

// TestSandboxExecLauncher_ProfileContent asserts the SBPL profile contains
// all required clauses from the brief.
func TestSandboxExecLauncher_ProfileContent(t *testing.T) {
	scratch := t.TempDir()
	const run = "true"

	f := &sandboxExecFactory{}
	l := f.buildForRun(run).(sandboxExecLauncher)
	argv := l.prepend(scratch, nil)

	// Find -f <path> in argv.
	profilePath := ""
	for i, a := range argv {
		if a == "-f" && i+1 < len(argv) {
			profilePath = argv[i+1]
			break
		}
	}
	if profilePath == "" {
		t.Fatal("argv missing -f <profile>")
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile %q: %v", profilePath, err)
	}
	profile := string(data)

	required := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (subpath (param "SCRATCH")))`,
		`(allow file-write* (subpath (param "TMPDIR")))`,
		`(allow file-write* (literal "/dev/null"))`,
	}
	for _, clause := range required {
		if !strings.Contains(profile, clause) {
			t.Errorf("profile missing clause %q; full profile:\n%s", clause, profile)
		}
	}
}

// TestDetectPlatformSandbox_SandboxExecFound asserts that when sandbox-exec is
// found by lookPath, detectPlatformSandbox returns a non-nil factory + label.
func TestDetectPlatformSandbox_SandboxExecFound(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "sandbox-exec" {
			return "/usr/bin/sandbox-exec", nil
		}
		return "", os.ErrNotExist
	}

	l, label := detectPlatformSandbox(lookPath)
	if l == nil {
		t.Fatal("detectPlatformSandbox with sandbox-exec: launcher = nil, want non-nil")
	}
	if !strings.Contains(strings.ToLower(label), "sandbox") {
		t.Errorf("label = %q, want to contain \"sandbox\"", label)
	}

	// The returned launcher must be a factory and build a non-nil argv.
	factory, ok := l.(sandboxLauncherFactory)
	if !ok {
		t.Fatalf("launcher does not implement sandboxLauncherFactory: %T", l)
	}
	perRun := factory.buildForRun("true")
	argv := perRun.prepend(t.TempDir(), nil)
	if argv == nil {
		t.Error("sandbox-exec launcher.prepend returned nil")
	}
}

// TestDetectPlatformSandbox_NotFound asserts that when sandbox-exec is absent,
// detectPlatformSandbox returns (nil, "") triggering the no-op fallback.
func TestDetectPlatformSandbox_NotFound(t *testing.T) {
	lookPath := func(_ string) (string, error) {
		return "", os.ErrNotExist
	}
	l, label := detectPlatformSandbox(lookPath)
	if l != nil {
		t.Errorf("detectPlatformSandbox without sandbox-exec: got %T, want nil", l)
	}
	if label != "" {
		t.Errorf("label = %q, want \"\"", label)
	}
}
