//go:build linux

package native

// WHITE-BOX Linux sandbox arg-construction tests (//go:build linux).
// No kernel, bwrap binary, or Landlock ABI needed — these test argv
// assembly only (pure function of inputs).
//
// cve-runner-pending: live isolation tests are in sandbox_linux_integ_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
)

// TestBwrapLauncher_ArgvStructure asserts the bubblewrap launcher builds
// the correct argv per the brief (exact order matters for bwrap):
//
//	bwrap --tmpfs <HOME> --ro-bind-try <cred> <cred> …
//	      --bind <scratch> <scratch> --ro-bind /usr /usr --ro-bind /bin /bin
//	      --ro-bind /lib /lib --ro-bind-try /lib64 /lib64 --ro-bind /etc /etc
//	      --tmpfs /tmp --proc /proc --dev /dev --chdir <scratch>
//	      --unshare-pid --die-with-parent --new-session -- sh -c <run>
func TestBwrapLauncher_ArgvStructure(t *testing.T) {
	const bwrapPath = "/usr/bin/bwrap"
	const scratch = "/tmp/awf-run-abc"
	const home = "/home/runner"
	const run = "echo hello"

	l := bwrapLauncher{bwrapPath: bwrapPath, home: home, run: run}
	creds := []string{home + "/.claude", home + "/.factory"}
	argv := l.prepend(scratch, nil, creds)

	if len(argv) == 0 {
		t.Fatal("bwrapLauncher.prepend: empty argv")
	}
	if argv[0] != bwrapPath {
		t.Errorf("argv[0] = %q, want %q", argv[0], bwrapPath)
	}

	// --tmpfs <HOME> must appear BEFORE any --ro-bind-try for cred dirs.
	tmpfsHomeIdx := -1
	for i, a := range argv {
		if a == "--tmpfs" && i+1 < len(argv) && argv[i+1] == home {
			tmpfsHomeIdx = i
			break
		}
	}
	if tmpfsHomeIdx < 0 {
		t.Errorf("argv missing --tmpfs %s; got %v", home, argv)
	}

	// All cred dirs must appear as --ro-bind-try <d> <d> AFTER --tmpfs HOME.
	for _, cred := range creds {
		idx := -1
		for i, a := range argv {
			if a == "--ro-bind-try" && i+1 < len(argv) && argv[i+1] == cred {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("argv missing --ro-bind-try %s; got %v", cred, argv)
			continue
		}
		if idx < tmpfsHomeIdx {
			t.Errorf("--ro-bind-try %s (idx=%d) appears BEFORE --tmpfs HOME (idx=%d)", cred, idx, tmpfsHomeIdx)
		}
		// dest must equal src for bind mounts
		if argv[idx+2] != cred {
			t.Errorf("--ro-bind-try %s <dest>: want %s got %s", cred, cred, argv[idx+2])
		}
	}

	// --bind <scratch> <scratch> must appear.
	bindScratchIdx := -1
	for i, a := range argv {
		if a == "--bind" && i+2 < len(argv) && argv[i+1] == scratch && argv[i+2] == scratch {
			bindScratchIdx = i
			break
		}
	}
	if bindScratchIdx < 0 {
		t.Errorf("argv missing --bind %s %s; got %v", scratch, scratch, argv)
	}

	// --chdir <scratch> must appear.
	chdirIdx := -1
	for i, a := range argv {
		if a == "--chdir" && i+1 < len(argv) && argv[i+1] == scratch {
			chdirIdx = i
			break
		}
	}
	if chdirIdx < 0 {
		t.Errorf("argv missing --chdir %s; got %v", scratch, argv)
	}

	// Must end with -- sh -c <run>.
	n := len(argv)
	if n < 4 || argv[n-4] != "--" || argv[n-3] != "sh" || argv[n-2] != "-c" || argv[n-1] != run {
		t.Errorf("argv tail: want [-- sh -c %q], got %v", run, argv[n-4:])
	}
}

// TestBwrapLauncher_SystemDirs asserts the required system ro-bind mounts
// are present: /usr /bin /lib /etc; /lib64 as --ro-bind-try.
func TestBwrapLauncher_SystemDirs(t *testing.T) {
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: "/home/u", run: "true"}
	argv := l.prepend("/tmp/s", nil, nil)

	required := []struct{ flag, path string }{
		{"--ro-bind", "/usr"},
		{"--ro-bind", "/bin"},
		{"--ro-bind", "/lib"},
		{"--ro-bind", "/etc"},
	}
	for _, r := range required {
		found := false
		for i, a := range argv {
			if a == r.flag && i+1 < len(argv) && argv[i+1] == r.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv missing %s %s; got %v", r.flag, r.path, argv)
		}
	}

	// /lib64 must be --ro-bind-try (may not exist on all distros).
	lib64Found := false
	for i, a := range argv {
		if a == "--ro-bind-try" && i+1 < len(argv) && argv[i+1] == "/lib64" {
			lib64Found = true
			break
		}
	}
	if !lib64Found {
		t.Errorf("argv missing --ro-bind-try /lib64; got %v", argv)
	}

	// /tmp must be --tmpfs /tmp.
	tmpfsFound := false
	for i, a := range argv {
		if a == "--tmpfs" && i+1 < len(argv) && argv[i+1] == "/tmp" {
			tmpfsFound = true
			break
		}
	}
	if !tmpfsFound {
		t.Errorf("argv missing --tmpfs /tmp; got %v", argv)
	}

	// --proc /proc and --dev /dev.
	for _, pair := range [][]string{{"--proc", "/proc"}, {"--dev", "/dev"}} {
		found := false
		for i, a := range argv {
			if a == pair[0] && i+1 < len(argv) && argv[i+1] == pair[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv missing %s %s; got %v", pair[0], pair[1], argv)
		}
	}
}

// TestBwrapLauncher_IsolationFlags asserts --unshare-pid, --die-with-parent,
// --new-session are present.
func TestBwrapLauncher_IsolationFlags(t *testing.T) {
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: "/home/u", run: "true"}
	argv := l.prepend("/tmp/s", nil, nil)
	argSet := make(map[string]bool, len(argv))
	for _, a := range argv {
		argSet[a] = true
	}
	for _, flag := range []string{"--unshare-pid", "--die-with-parent", "--new-session"} {
		if !argSet[flag] {
			t.Errorf("argv missing %s; got %v", flag, argv)
		}
	}
}

// TestTrampolineLauncher_ArgvStructure asserts the landlock-trampoline
// launcher builds argv = [self, "__sandbox", <policyJSON>, "--", "sh", "-c", run].
func TestTrampolineLauncher_ArgvStructure(t *testing.T) {
	const self = "/proc/self/exe"
	const scratch = "/tmp/awf-run-xyz"
	const run = "echo world"

	l := trampolineLauncher{self: self, run: run}
	creds := []string{"/home/u/.claude", "/home/u/.factory"}
	argv := l.prepend(scratch, nil, creds)

	if len(argv) < 7 {
		t.Fatalf("trampoline argv too short: %v", argv)
	}

	if argv[0] != self {
		t.Errorf("argv[0] = %q, want self=%q", argv[0], self)
	}
	if argv[1] != "__sandbox" {
		t.Errorf("argv[1] = %q, want \"__sandbox\"", argv[1])
	}

	// argv[2] must be valid JSON encoding of sandboxPolicy.
	var p SandboxPolicy
	if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
		t.Fatalf("argv[2] is not valid sandboxPolicy JSON: %v\n  got: %s", err, argv[2])
	}

	// RODirs must include cred dirs AND system read-only dirs.
	roDirSet := make(map[string]bool, len(p.RODirs))
	for _, d := range p.RODirs {
		roDirSet[d] = true
	}
	for _, cred := range creds {
		if !roDirSet[cred] {
			t.Errorf("policy.RODirs missing cred dir %q; RODirs=%v", cred, p.RODirs)
		}
	}
	// /proc and /sys are part of the fixed system RO set: Bun (the claude
	// runtime) reads /proc on startup, and the bwrap path already grants
	// --proc /proc, so the landlock path must match for parity.
	for _, sysDir := range []string{"/usr", "/bin", "/lib", "/etc", "/proc", "/sys"} {
		if !roDirSet[sysDir] {
			t.Errorf("policy.RODirs missing system dir %q; RODirs=%v", sysDir, p.RODirs)
		}
	}

	// RWDirs must include scratch, /tmp, AND /dev. /dev is load-bearing: Bun
	// aborts on startup if it cannot open /dev/urandom (entropy) or /dev/null,
	// which is why a bwrap-less container needs the landlock policy to grant it
	// (the bwrap path already does, via --dev /dev).
	rwDirSet := make(map[string]bool, len(p.RWDirs))
	for _, d := range p.RWDirs {
		rwDirSet[d] = true
	}
	if !rwDirSet[scratch] {
		t.Errorf("policy.RWDirs missing scratch %q; RWDirs=%v", scratch, p.RWDirs)
	}
	if !rwDirSet["/tmp"] {
		t.Errorf("policy.RWDirs missing /tmp; RWDirs=%v", p.RWDirs)
	}
	if !rwDirSet["/dev"] {
		t.Errorf("policy.RWDirs missing /dev; RWDirs=%v", p.RWDirs)
	}

	// separator and sh -c run at the tail.
	if argv[3] != "--" {
		t.Errorf("argv[3] = %q, want \"--\"", argv[3])
	}
	if argv[4] != "sh" || argv[5] != "-c" || argv[6] != run {
		t.Errorf("argv[4:7] = %v, want [sh -c %q]", argv[4:7], run)
	}
}

// TestTrampolineLauncher_PolicyJSONRoundtrip asserts that the policy JSON
// in argv[2] encodes and decodes RODirs/RWDirs without loss.
func TestTrampolineLauncher_PolicyJSONRoundtrip(t *testing.T) {
	const self = "/usr/local/bin/awf"
	const scratch = "/tmp/awf-rt"
	const run = "true"

	l := trampolineLauncher{self: self, run: run}
	creds := []string{"/home/u/.claude", "/home/u/.codex"}
	argv := l.prepend(scratch, nil, creds)
	if len(argv) < 3 {
		t.Fatalf("argv too short: %v", argv)
	}

	var p SandboxPolicy
	if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	// Run must round-trip through the policy.
	if p.Run != run {
		t.Errorf("policy.Run = %q, want %q", p.Run, run)
	}

	// RODirs and RWDirs must be non-empty slices.
	if len(p.RODirs) == 0 {
		t.Error("policy.RODirs is empty")
	}
	if len(p.RWDirs) == 0 {
		t.Error("policy.RWDirs is empty")
	}
}

// TestCredDirs_EmptyHomeSkipsRelativePaths asserts that an empty runHome
// does not produce relative or empty paths in credDirs output (guard fix).
func TestCredDirs_EmptyHomeSkipsRelativePaths(t *testing.T) {
	// Clear CODEX_HOME so we exercise the default filepath.Join branch.
	t.Setenv("CODEX_HOME", "")
	dirs := credDirs("") // empty runHome
	for _, d := range dirs {
		if d == "" {
			t.Errorf("credDirs(\"\") returned empty string in %v", dirs)
		}
		if !strings.HasPrefix(d, "/") {
			t.Errorf("credDirs(\"\") returned non-absolute path %q in %v", d, dirs)
		}
	}
}

func TestDetectLinuxSandboxRequiresUsableBwrap(t *testing.T) {
	errBroken := os.ErrPermission
	tests := []struct {
		name      string
		probeErr  error
		landlock  int
		wantLabel string
		wantType  string
	}{
		{name: "usable bwrap wins", landlock: 1, wantLabel: "bwrap", wantType: "*native.bwrapLauncherFactory"},
		{name: "broken bwrap falls back to landlock", probeErr: errBroken, landlock: 1, wantLabel: "landlock-trampoline", wantType: "*native.trampolineLauncherFactory"},
		{name: "broken bwrap and no landlock falls back to no-op caller", probeErr: errBroken, wantLabel: "", wantType: "<nil>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probed string
			launcher, label := detectLinuxSandbox(
				func(name string) (string, error) {
					if name != "bwrap" {
						t.Fatalf("lookPath name = %q, want bwrap", name)
					}
					return "/fake/bwrap", nil
				},
				func(path string) error { probed = path; return tt.probeErr },
				func() (int, error) { return tt.landlock, nil },
			)
			if probed != "/fake/bwrap" {
				t.Fatalf("probe path = %q, want looked-up path", probed)
			}
			if label != tt.wantLabel {
				t.Errorf("label = %q, want %q", label, tt.wantLabel)
			}
			if got := fmt.Sprintf("%T", launcher); got != tt.wantType {
				t.Errorf("launcher type = %q, want %q", got, tt.wantType)
			}
		})
	}
}

func TestLinuxSandboxPoliciesUseAbsoluteWorkdir(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	b, err := New("state/work")
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatal(err)
	}

	bwrapArgv := (bwrapLauncher{bwrapPath: "/bwrap", run: "true"}).prepend(h.ID, nil, nil)
	for i, arg := range bwrapArgv {
		if (arg == "--bind" || arg == "--chdir") && !filepath.IsAbs(bwrapArgv[i+1]) {
			t.Errorf("bwrap %s path = %q, want absolute", arg, bwrapArgv[i+1])
		}
	}

	landlockArgv := (trampolineLauncher{self: "/awf", run: "true"}).prepend(h.ID, nil, nil)
	var policy SandboxPolicy
	if err := json.Unmarshal([]byte(landlockArgv[2]), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.RWDirs) == 0 || !filepath.IsAbs(policy.RWDirs[0]) {
		t.Fatalf("Landlock scratch RW path = %q, want absolute", policy.RWDirs[0])
	}
}

// TestBwrapLauncher_WritableCredDirs asserts a writable agent config dir gets a
// --bind-try (read-write) AFTER its --ro-bind-try baseline (so it overrides),
// while the ~/.config catch-all (RO only) gets --ro-bind-try and never --bind-try.
// This is the T2 token-refresh-persistence grant with the XDG-catch-all guard.
func TestBwrapLauncher_WritableCredDirs(t *testing.T) {
	const home = "/home/runner"
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: home, run: "true"}
	argv := l.prepend("/tmp/s", []string{home + "/.factory"}, []string{home + "/.factory", home + "/.config"})

	triple := func(flag, d string) int {
		for i := 0; i+2 < len(argv); i++ {
			if argv[i] == flag && argv[i+1] == d && argv[i+2] == d {
				return i
			}
		}
		return -1
	}
	roIdx := triple("--ro-bind-try", home+"/.factory")
	rwIdx := triple("--bind-try", home+"/.factory")
	if roIdx < 0 {
		t.Errorf("~/.factory missing --ro-bind-try baseline; argv=%v", argv)
	}
	if rwIdx < 0 {
		t.Errorf("~/.factory missing --bind-try (writable override); argv=%v", argv)
	}
	if roIdx >= 0 && rwIdx >= 0 && rwIdx < roIdx {
		t.Errorf("--bind-try ~/.factory (%d) must come after --ro-bind-try (%d) to override; argv=%v", rwIdx, roIdx, argv)
	}
	if triple("--bind-try", home+"/.config") >= 0 {
		t.Errorf("~/.config catch-all must NOT be writable (--bind-try present); argv=%v", argv)
	}
	if triple("--ro-bind-try", home+"/.config") < 0 {
		t.Errorf("~/.config missing --ro-bind-try; argv=%v", argv)
	}
}

// TestTrampolineLauncher_WritableCredDirsInPolicy asserts writable cred dirs land
// in SandboxPolicy.RWDirs (Landlock read+write) beside the fixed set, while the
// ~/.config catch-all is only in RODirs.
func TestTrampolineLauncher_WritableCredDirsInPolicy(t *testing.T) {
	l := trampolineLauncher{self: "/awf", run: "true"}
	argv := l.prepend("/tmp/s", []string{"/home/u/.factory"}, []string{"/home/u/.factory", "/home/u/.config"})

	var p SandboxPolicy
	if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
		t.Fatalf("policy JSON: %v; argv=%v", err, argv)
	}
	if !hasDir(p.RWDirs, "/home/u/.factory") {
		t.Errorf("RWDirs missing writable cred dir; RWDirs=%v", p.RWDirs)
	}
	if hasDir(p.RWDirs, "/home/u/.config") {
		t.Errorf("RWDirs must NOT contain the ~/.config catch-all; RWDirs=%v", p.RWDirs)
	}
	for _, fixed := range []string{"/tmp/s", "/tmp", "/dev"} {
		if !hasDir(p.RWDirs, fixed) {
			t.Errorf("RWDirs missing fixed writable %q; RWDirs=%v", fixed, p.RWDirs)
		}
	}
	if !hasDir(p.RODirs, "/home/u/.config") {
		t.Errorf("RODirs missing ~/.config catch-all; RODirs=%v", p.RODirs)
	}
}

// TestSystemRODirs_IncludesOpt asserts /opt is granted read+execute. node/npm
// global prefixes live there and an agent CLI shim in /usr/local/bin routinely
// resolves into it (verified: codex -> /opt/node-*/lib/node_modules/@openai/codex,
// run by /opt/node-*/bin/node), so omitting it yields the opaque exit 126.
func TestSystemRODirs_IncludesOpt(t *testing.T) {
	if !hasDir(systemRODirs, "/opt") {
		t.Errorf("systemRODirs missing /opt; got %v", systemRODirs)
	}
}

// TestBwrapLauncher_OptBound asserts the bwrap argv binds /opt read-only, with
// the -try variant so hosts without /opt are unaffected.
func TestBwrapLauncher_OptBound(t *testing.T) {
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: "/home/r", run: "true"}
	argv := l.prepend("/tmp/s", nil, nil)

	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--ro-bind-try" && argv[i+1] == "/opt" && argv[i+2] == "/opt" {
			return
		}
	}
	t.Errorf("bwrap argv missing --ro-bind-try /opt /opt; argv=%v", argv)
}

// Threat-model alignment (2026-08-16): open reads by default (write-only
// confinement, SECURITY.md); AWF_SANDBOX_READS=confined restores the
// deny-by-default read policy.
func TestTrampolineLauncher_OpenReadsPolicy(t *testing.T) {
	l := trampolineLauncher{self: "/bin/awf", run: "echo hi", readsOpen: true}
	argv := l.prepend("/scratch", []string{"/home/u/.codex"}, []string{"/home/u/.claude"})
	var p SandboxPolicy
	if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
		t.Fatalf("policy JSON: %v", err)
	}
	if len(p.RODirs) != 1 || p.RODirs[0] != "/" {
		t.Fatalf("open reads: RODirs = %v, want [/]", p.RODirs)
	}
	if p.ROFiles != nil {
		t.Fatalf("open reads need no file grants (resolv.conf is under /), got %v", p.ROFiles)
	}
	// writes stay confined: scratch + /tmp + /dev + writable cred dirs
	for _, want := range []string{"/scratch", "/tmp", "/dev", "/home/u/.codex"} {
		if !slices.Contains(p.RWDirs, want) {
			t.Errorf("RWDirs missing %q: %v", want, p.RWDirs)
		}
	}
}

func TestTrampolineLauncher_ConfinedReadsPolicyUnchanged(t *testing.T) {
	l := trampolineLauncher{self: "/bin/awf", run: "echo hi", readsOpen: false}
	argv := l.prepend("/scratch", []string{"/home/u/.codex"}, []string{"/home/u/.claude"})
	var p SandboxPolicy
	if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
		t.Fatalf("policy JSON: %v", err)
	}
	if slices.Contains(p.RODirs, "/") {
		t.Fatalf("confined reads must NOT grant /, got %v", p.RODirs)
	}
	if !slices.Contains(p.RODirs, "/etc") || !slices.Contains(p.RODirs, "/home/u/.claude") {
		t.Fatalf("confined reads lost the system/cred grants: %v", p.RODirs)
	}
}

func TestBwrapLauncher_OpenReadsArgv(t *testing.T) {
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: "/home/u", run: "echo hi", readsOpen: true}
	argv := l.prepend("/scratch", []string{"/home/u/.codex"}, []string{"/home/u/.claude"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind / /") {
		t.Errorf("open reads must bind the whole OS read-only:\n%s", joined)
	}
	if strings.Contains(joined, "--tmpfs /home/u") {
		t.Errorf("open reads must NOT shadow HOME:\n%s", joined)
	}
	// writes still confined: scratch is rw, /tmp is a fresh tmpfs
	if !strings.Contains(joined, "--bind /scratch /scratch") || !strings.Contains(joined, "--tmpfs /tmp") {
		t.Errorf("write confinement lost:\n%s", joined)
	}
}

func TestBwrapLauncher_ConfinedReadsArgvUnchanged(t *testing.T) {
	l := bwrapLauncher{bwrapPath: "/usr/bin/bwrap", home: "/home/u", run: "echo hi", readsOpen: false}
	argv := l.prepend("/scratch", []string{"/home/u/.codex"}, []string{"/home/u/.claude"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--tmpfs /home/u") {
		t.Errorf("confined reads keep the HOME tmpfs:\n%s", joined)
	}
	if strings.Contains(joined, "--ro-bind / /") {
		t.Errorf("confined reads must NOT bind / read-only:\n%s", joined)
	}
}
