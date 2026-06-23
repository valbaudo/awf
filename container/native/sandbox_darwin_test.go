//go:build darwin

package native

// WHITE-BOX darwin sandbox arg-construction tests (//go:build darwin).
// No sandbox-exec binary or actual sandboxing needed — these test argv
// assembly and SBPL profile generation only (pure function of inputs).
//
// Live isolation tests are in sandbox_darwin_integ_test.go.

import (
	"os"
	osexec "os/exec"
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
//	 "-D", "AWFOUT=<awfout>", "-f", "<profile.sb>", "--", "sh", "-c", <run>]
func TestSandboxExecLauncher_ArgvStructure(t *testing.T) {
	scratch := t.TempDir()
	const run = "echo hello"

	f := &sandboxExecFactory{}
	l := f.buildForRun(run).(sandboxExecLauncher)
	argv := l.prepend(scratch, nil)

	if len(argv) < 13 {
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
	// argv[5] == "-D", argv[6] == "AWFOUT=<dir>"
	if argv[5] != "-D" {
		t.Errorf("argv[5] = %q, want \"-D\"", argv[5])
	}
	if !strings.HasPrefix(argv[6], "AWFOUT=") {
		t.Errorf("argv[6] = %q, want AWFOUT=...", argv[6])
	}
	// argv[7] == "-f", argv[8] == <profile path>
	if argv[7] != "-f" {
		t.Errorf("argv[7] = %q, want \"-f\"", argv[7])
	}
	profilePath := argv[8]
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
	// argv[9] == "--", argv[10] == "sh", argv[11] == "-c", argv[12] == run
	if argv[9] != "--" {
		t.Errorf("argv[9] = %q, want \"--\"", argv[9])
	}
	if argv[10] != "sh" || argv[11] != "-c" || argv[12] != run {
		t.Errorf("argv[9:13] = %v, want [-- sh -c %q]", argv[9:], run)
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
		`(allow file-write* (subpath (param "AWFOUT")))`,
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

// TestSandboxExecLauncher_FailClosed_ProfileWriteFailure asserts that when the
// profile temp file cannot be written (unwritable/nonexistent tmpDir), prepend
// returns a non-nil fail-closed sentinel argv rather than nil. Returning nil
// would cause exec.go to run the step unsandboxed on the host (fail-OPEN).
//
// The test uses tmpDirOverride to inject a guaranteed-unwritable path, then
// checks:
//  1. The returned argv is non-nil (no fail-open).
//  2. The script text contains the expected error message.
//  3. Actually running the argv exits non-zero (the sentinel does what it says).
func TestSandboxExecLauncher_FailClosed_ProfileWriteFailure(t *testing.T) {
	// Use a nonexistent directory as tmpDirOverride so os.CreateTemp always fails.
	unwritable := t.TempDir() + "/does-not-exist"

	l := sandboxExecLauncher{run: "echo should-not-run", tmpDirOverride: unwritable}
	argv := l.prepend(t.TempDir(), nil)

	// MUST be non-nil — nil means exec.go falls through to bare sh (fail-open).
	if argv == nil {
		t.Fatal("prepend with unwritable tmpDir returned nil: step would run UNSANDBOXED (fail-open bug)")
	}

	// The sentinel script must mention "refusing to run step unsandboxed".
	script := strings.Join(argv, " ")
	if !strings.Contains(script, "refusing to run step unsandboxed") {
		t.Errorf("sentinel argv does not contain expected message; got: %v", argv)
	}

	// Running the sentinel must exit non-zero (the step is refused, not silently dropped).
	if len(argv) < 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("unexpected sentinel argv shape: %v", argv)
	}
	cmd := argv[2] // the sh -c script
	_ = cmd        // executed via os/exec below
	out, exitErr := runSentinel(t, argv)
	if exitErr == nil {
		t.Errorf("sentinel exited 0; want non-zero exit. stdout+stderr:\n%s", out)
	}
}

// TestSandboxExecLauncher_NormalPath_NotFailClosed asserts that with a writable
// tmpDir, prepend returns the normal sandbox-exec argv (not the sentinel).
func TestSandboxExecLauncher_NormalPath_NotFailClosed(t *testing.T) {
	l := sandboxExecLauncher{run: "echo hello"}
	argv := l.prepend(t.TempDir(), nil)

	if argv == nil {
		t.Fatal("prepend with writable tmpDir returned nil")
	}
	if len(argv) == 0 || argv[0] != "sandbox-exec" {
		t.Errorf("expected normal sandbox-exec argv; got: %v", argv)
	}
}

// runSentinel runs argv[0] argv[1:] via os/exec and returns combined
// stdout+stderr plus the error. It is only used to assert exit-code behaviour.
func runSentinel(t *testing.T, argv []string) (string, error) {
	t.Helper()
	cmd := osexec.Command(argv[0], argv[1:]...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	return string(out), err
}
