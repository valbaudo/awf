//go:build linux

package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// detectPlatformSandbox returns the best available sandbox launcher for the
// Linux platform. Selection order:
//
//  1. bwrap — if found via lookPath and its namespace/mount capability probe
//     succeeds → bwrapLauncherFactory (stored in Backend.sandbox); exec.go
//     calls buildForRun per dispatch.
//  2. go-landlock — if the kernel supports Landlock ABI ≥1 (probed via
//     LandlockGetABIVersion, a non-mutating syscall) → trampolineLauncherFactory.
//  3. Neither — return (nil, "") so detectSandbox falls back to no-op + warn.
func detectPlatformSandbox(lookPath func(string) (string, error)) (sandboxLauncher, string) {
	return detectLinuxSandbox(lookPath, probeBwrapSandbox, llsyscall.LandlockGetABIVersion)
}

// detectLinuxSandbox is the deterministic Linux selection core. A bwrap
// executable is eligible only if it can actually establish the policy AWF
// relies on; an installed-but-blocked binary is treated as unavailable.
func detectLinuxSandbox(
	lookPath func(string) (string, error),
	probeBwrap func(string) error,
	landlockABIVersion func() (int, error),
) (sandboxLauncher, string) {
	// ── 1. bubblewrap ────────────────────────────────────────────────────────
	if bwrapPath, err := lookPath("bwrap"); err == nil && bwrapPath != "" {
		if err := probeBwrap(bwrapPath); err == nil {
			home, _ := os.UserHomeDir()
			return &bwrapLauncherFactory{bwrapPath: bwrapPath, home: home}, "bwrap"
		}
	}

	// ── 2. go-landlock trampoline ─────────────────────────────────────────────
	// LandlockGetABIVersion is a pure probe (landlock_get_abi(2)); it does not
	// create a ruleset or restrict the process. We require ABI ≥ 1 (Linux 5.13).
	if ver, err := landlockABIVersion(); err == nil && ver >= 1 {
		self, _ := os.Executable()
		return &trampolineLauncherFactory{self: self}, "landlock-trampoline"
	}

	// ── 3. no platform sandbox ────────────────────────────────────────────────
	return nil, ""
}

func probeBwrapSandbox(bwrapPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bwrapPath,
		"--ro-bind", "/", "/",
		"--unshare-pid",
		"--die-with-parent",
		"--new-session",
		"--", "/bin/true",
	).Run()
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
func (f *bwrapLauncherFactory) prepend(_ string, _, _ []string) []string { return nil }

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
func (l bwrapLauncher) prepend(scratchDir string, rwDirs, roDirs []string) []string {
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

	// Re-bind the agent config dirs that must persist a token refresh READ-WRITE,
	// overriding their read-only bind above (a later bwrap bind on the same path
	// shadows the earlier one). --bind-try skips absent dirs. Bare ~/.config is in
	// roDirs only (see credDirsWritable), so it is never writable.
	for _, d := range rwDirs {
		argv = append(argv, "--bind-try", d, d)
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
	// /opt is where node/npm global prefixes commonly live, and an agent CLI
	// shim in /usr/local/bin routinely resolves into it (verified: codex ->
	// /opt/node-*/lib/node_modules/@openai/codex, interpreter /opt/node-*/bin/node).
	// Without it the shim execs but its real script/interpreter is unreachable.
	// -try: /opt is absent on many hosts.
	argv = append(argv, "--ro-bind-try", "/opt", "/opt")

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

func (f *trampolineLauncherFactory) prepend(_ string, _, _ []string) []string { return nil }

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
//
// /proc and /sys are included because runtimes like Bun (the claude-code CLI)
// read them on startup; without /proc the process aborts before running. The
// bwrap path supplies /proc via `--proc /proc`, so granting it here keeps the
// two sandbox backends at parity. (/dev is granted read-WRITE — see the
// trampoline's RWDirs below — because Bun must open /dev/urandom and /dev/null.)
// /opt is included because node/npm global prefixes commonly live there, and an
// agent CLI shim in /usr/local/bin routinely resolves into it (verified: codex ->
// /opt/node-*/lib/node_modules/@openai/codex, run by /opt/node-*/bin/node).
// Without it the confined step dies with an opaque exit 126. RestrictPaths uses
// IgnoreIfMissing(), so hosts without /opt are unaffected.
var systemRODirs = []string{"/usr", "/bin", "/lib", "/lib64", "/etc", "/proc", "/sys", "/opt"}

// prepend returns the argv:
//
//	[self, "__sandbox", <policyJSON>, "--", "sh", "-c", run]
//
// exec.go runs: exec.CommandContext(ctx, argv[0], argv[1:]...)
// This re-execs the awf binary. cmd/awf/main.go detects "__sandbox" in
// maybeSandboxTrampoline() and applies the Landlock policy before
// syscall.Exec("/bin/sh",...).
func (l trampolineLauncher) prepend(scratchDir string, rwDirs, roDirs []string) []string {
	// RODirs = caller-supplied cred dirs + fixed system dirs.
	allRO := make([]string, 0, len(roDirs)+len(systemRODirs))
	allRO = append(allRO, roDirs...)
	allRO = append(allRO, systemRODirs...)

	// RWDirs = the fixed writable set + the agent config dirs that must persist a
	// token refresh (credDirsWritable). RestrictPaths uses IgnoreIfMissing()
	// (sandbox_landlock_linux.go), so absent dirs are skipped; a path present in
	// both RODirs and RWDirs gets the union — read+write.
	allRW := make([]string, 0, 3+len(rwDirs))
	// /dev is read-write: Bun (claude-code) opens /dev/urandom for entropy and
	// writes to /dev/null on startup, and aborts if it cannot. The bwrap path
	// provides this via `--dev /dev`; the landlock path must grant it explicitly
	// or the confined process never starts.
	allRW = append(allRW, scratchDir, "/tmp", "/dev")
	allRW = append(allRW, rwDirs...)

	p := SandboxPolicy{
		RODirs: allRO,
		RWDirs: allRW,
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
