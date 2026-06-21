//go:build linux

package native

import (
	"encoding/json"
	"os"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// detectPlatformSandbox returns the best available sandbox launcher for the
// Linux platform. Selection order:
//
//  1. bwrap — if found via lookPath → bwrapLauncherFactory (stored in
//     Backend.sandbox); exec.go calls buildForRun per dispatch.
//  2. go-landlock — if the kernel supports Landlock ABI ≥1 (probed via
//     LandlockGetABIVersion, a non-mutating syscall) → trampolineLauncherFactory.
//  3. Neither — return (nil, "") so detectSandbox falls back to no-op + warn.
func detectPlatformSandbox(lookPath func(string) (string, error)) (sandboxLauncher, string) {
	// ── 1. bubblewrap ────────────────────────────────────────────────────────
	if bwrapPath, err := lookPath("bwrap"); err == nil && bwrapPath != "" {
		home, _ := os.UserHomeDir()
		return &bwrapLauncherFactory{bwrapPath: bwrapPath, home: home}, "bwrap"
	}

	// ── 2. go-landlock trampoline ─────────────────────────────────────────────
	// LandlockGetABIVersion is a pure probe (landlock_get_abi(2)); it does not
	// create a ruleset or restrict the process. We require ABI ≥ 1 (Linux 5.13).
	if ver, err := llsyscall.LandlockGetABIVersion(); err == nil && ver >= 1 {
		self, _ := os.Executable()
		return &trampolineLauncherFactory{self: self}, "landlock-trampoline"
	}

	// ── 3. no platform sandbox ────────────────────────────────────────────────
	return nil, ""
}

// ─── bubblewrap launcher ─────────────────────────────────────────────────────

// bwrapLauncherFactory is stored in Backend.sandbox when bwrap is available.
// exec.go calls buildForRun(cmd.Run) per dispatch to get a bwrapLauncher with
// the run-command embedded.
type bwrapLauncherFactory struct {
	bwrapPath string
	home      string
}

// prepend is a sentinel no-op. exec.go always calls buildForRun first.
func (f *bwrapLauncherFactory) prepend(_ string, _ []string) []string { return nil }

func (f *bwrapLauncherFactory) buildForRun(run string) sandboxLauncher {
	return bwrapLauncher{bwrapPath: f.bwrapPath, home: f.home, run: run}
}

// bwrapLauncher builds the bubblewrap argv for one dispatch.
// Constructed fresh per dispatch via bwrapLauncherFactory.buildForRun.
type bwrapLauncher struct {
	bwrapPath string
	home      string
	run       string
}

// prepend returns the complete bwrap argv including the terminal "-- sh -c <run>".
// exec.go runs: exec.CommandContext(ctx, argv[0], argv[1:]...)
//
// Argv recipe (per brief, order is load-bearing):
//
//	bwrap
//	  --tmpfs <HOME>
//	  --ro-bind-try <cred> <cred> ...   (each cred dir, order preserved)
//	  --bind <scratch> <scratch>
//	  --ro-bind /usr /usr  --ro-bind /bin /bin  --ro-bind /lib /lib
//	  --ro-bind-try /lib64 /lib64
//	  --ro-bind /etc /etc
//	  --tmpfs /tmp  --proc /proc  --dev /dev
//	  --chdir <scratch>
//	  --unshare-pid  --die-with-parent  --new-session
//	  -- sh -c <run>
func (l bwrapLauncher) prepend(scratchDir string, roDirs []string) []string {
	argv := []string{l.bwrapPath}

	// Isolate HOME: overlay with tmpfs so the step cannot read real credentials.
	if l.home != "" {
		argv = append(argv, "--tmpfs", l.home)
	}

	// Re-bind each credential directory read-only inside the new mount namespace.
	// --ro-bind-try silently skips dirs that don't exist (new hosts, optional tools).
	for _, d := range roDirs {
		argv = append(argv, "--ro-bind-try", d, d)
	}

	// Bind the per-run scratch dir read-write so the step can write output files.
	argv = append(argv, "--bind", scratchDir, scratchDir)

	// Essential system dirs (read-only).
	for _, d := range []string{"/usr", "/bin", "/lib"} {
		argv = append(argv, "--ro-bind", d, d)
	}
	// /lib64 may not exist on all distros — use try variant.
	argv = append(argv, "--ro-bind-try", "/lib64", "/lib64")
	argv = append(argv, "--ro-bind", "/etc", "/etc")

	// /tmp as tmpfs (clean per-run), proc + dev pseudo-filesystems.
	argv = append(argv, "--tmpfs", "/tmp")
	argv = append(argv, "--proc", "/proc")
	argv = append(argv, "--dev", "/dev")

	// chdir into scratch so relative paths in the step resolve inside it.
	argv = append(argv, "--chdir", scratchDir)

	// PID namespace, death-on-parent-exit, new session.
	argv = append(argv, "--unshare-pid", "--die-with-parent", "--new-session")

	// Command separator then the actual shell command.
	argv = append(argv, "--", "sh", "-c", l.run)

	return argv
}

// ─── landlock-trampoline launcher ────────────────────────────────────────────

// trampolineLauncherFactory is stored in Backend.sandbox when go-landlock is
// available (and bwrap is not). exec.go calls buildForRun(cmd.Run) per dispatch.
type trampolineLauncherFactory struct {
	self string // path to the awf binary (os.Executable())
}

func (f *trampolineLauncherFactory) prepend(_ string, _ []string) []string { return nil }

func (f *trampolineLauncherFactory) buildForRun(run string) sandboxLauncher {
	return trampolineLauncher{self: f.self, run: run}
}

// trampolineLauncher builds the re-exec argv for one dispatch.
type trampolineLauncher struct {
	self string // path to the awf binary
	run  string // the shell command to exec inside the sandbox
}

// SandboxPolicy is the JSON payload the __sandbox trampoline subcommand decodes.
// It is exported so cmd/awf/sandbox_linux.go can decode it without duplicating
// the struct definition.
type SandboxPolicy struct {
	RODirs []string `json:"ro_dirs"`
	RWDirs []string `json:"rw_dirs"`
	Run    string   `json:"run"`
}

// systemRODirs are the fixed OS directories the trampoline always grants
// read-only access to (mirrors the bwrap --ro-bind list for the landlock case).
var systemRODirs = []string{"/usr", "/bin", "/lib", "/lib64", "/etc"}

// prepend returns the argv:
//
//	[self, "__sandbox", <policyJSON>, "--", "sh", "-c", run]
//
// exec.go runs: exec.CommandContext(ctx, argv[0], argv[1:]...)
// This re-execs the awf binary. cmd/awf/main.go detects "__sandbox" in
// maybeSandboxTrampoline() and applies the Landlock policy before
// syscall.Exec("/bin/sh",...).
func (l trampolineLauncher) prepend(scratchDir string, roDirs []string) []string {
	// RODirs = caller-supplied cred dirs + fixed system dirs.
	allRO := make([]string, 0, len(roDirs)+len(systemRODirs))
	allRO = append(allRO, roDirs...)
	allRO = append(allRO, systemRODirs...)

	p := SandboxPolicy{
		RODirs: allRO,
		RWDirs: []string{scratchDir, "/tmp"},
		Run:    l.run,
	}
	pJSON, err := json.Marshal(p)
	if err != nil {
		// Unreachable with a well-formed SandboxPolicy, but fail closed.
		panic("sandbox_linux: trampolineLauncher.prepend: JSON marshal: " + err.Error())
	}

	return []string{
		l.self, "__sandbox", string(pJSON), "--", "sh", "-c", l.run,
	}
}
