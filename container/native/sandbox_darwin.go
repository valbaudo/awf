//go:build darwin

package native

import (
	"os"
	"path/filepath"

	"github.com/valbaudo/awf/container"
)

// detectPlatformSandbox returns the best available sandbox launcher for macOS.
//
// Strategy: sandbox-exec(1) ships with every macOS installation. If it is
// found via lookPath, return a sandboxExecFactory + label "sandbox-exec".
// If it cannot be found (unusual — included for correctness), return (nil,"")
// so detectSandbox falls back to the no-op + warn path.
func detectPlatformSandbox(lookPath func(string) (string, error)) (sandboxLauncher, string) {
	if path, err := lookPath("sandbox-exec"); err == nil && path != "" {
		return &sandboxExecFactory{}, "sandbox-exec"
	}
	return nil, ""
}

// ─── sandboxExecFactory ───────────────────────────────────────────────────────

// sandboxExecFactory is stored in Backend.sandbox when sandbox-exec is
// available. exec.go calls buildForRun(cmd.Run) per dispatch to get a
// sandboxExecLauncher with the run-command embedded.
type sandboxExecFactory struct{}

// prepend is a factory-guard sentinel. exec.go type-asserts to
// sandboxLauncherFactory and always calls buildForRun first; this path is
// never reached in normal dispatch, so returning nil is safe here.
func (f *sandboxExecFactory) prepend(_ string, _, _ []string) []string { return nil }

func (f *sandboxExecFactory) buildForRun(run string) sandboxLauncher {
	return sandboxExecLauncher{run: run}
}

// ─── sandboxExecLauncher ──────────────────────────────────────────────────────

// sandboxExecLauncher builds the sandbox-exec(1) argv for one dispatch.
// Constructed fresh per dispatch via sandboxExecFactory.buildForRun.
//
// SBPL profile (per brief, plus /dev/null allow):
//
//	(version 1)
//	(allow default)              ; permit all by default (reads, network, etc.)
//	(allow process-exec)
//	(allow process-fork)
//	(deny file-write*)           ; deny all writes …
//	(allow file-write* (subpath (param "SCRATCH")))  ; … except the scratch dir
//	(allow file-write* (subpath (param "TMPDIR")))   ; … and TMPDIR
//	(allow file-write* (subpath (param "AWFOUT")))   ; … and the AWF_OUTPUT dir
//	(allow file-write* (literal "/dev/null"))         ; … and the discard device
//
// /dev/null is added beyond the brief minimum because `(deny file-write*)` also
// blocks writes to character devices, breaking common shell idioms like
// `command > /dev/null`. Since /dev/null discards all data it is not a
// security-sensitive target; permitting it avoids surprising step failures.
//
// Note: roDirs are intentionally ignored. (allow default) already grants
// read access to the entire host filesystem — no per-directory read rules
// are needed. Write isolation is the sole goal of this profile; per-directory
// RO grants would only matter if we used (deny default) instead.
//
// Temp file lifetime: the .sb profile is written to os.TempDir() and is NOT
// deleted inside prepend (it must remain readable when sandbox-exec launches).
// In v1 the file is left in TMPDIR until the OS clears it. Callers who want
// eager cleanup should wrap the Exec call and remove the file after the
// process exits (the path is not currently surfaced; a future iteration could
// return a cleanup func or embed the profile text directly via -p flag).
type sandboxExecLauncher struct {
	run string
	// tmpDirOverride is used in tests to inject an unwritable directory so that
	// profile-write failure can be forced deterministically. Empty means use os.TempDir().
	tmpDirOverride string
}

// failClosedArgv returns a sentinel argv that always exits non-zero (exit 125)
// with msg on stderr. Used by the sandbox launcher when profile-write fails so
// the step is loudly refused rather than silently run unsandboxed on the host.
func failClosedArgv(msg string) []string {
	return []string{"sh", "-c", "echo '" + msg + "' >&2; exit 125"}
}

// sbplProfile is the static SBPL sandbox profile. Parameters SCRATCH and TMPDIR
// are substituted at runtime via sandbox-exec -D flags.
const sbplProfile = `(version 1)
(allow default)
(allow process-exec)
(allow process-fork)
(deny file-write*)
(allow file-write* (subpath (param "SCRATCH")))
(allow file-write* (subpath (param "TMPDIR")))
(allow file-write* (subpath (param "AWFOUT")))
(allow file-write* (literal "/dev/null"))
`

// prepend returns the complete sandbox-exec argv including the terminal
// "-- sh -c <run>". exec.go runs: exec.CommandContext(ctx, argv[0], argv[1:]...)
//
// Argv shape:
//
//	["sandbox-exec",
//	  "-D", "SCRATCH=<realPath>",
//	  "-D", "TMPDIR=<realTmpdir>",
//	  "-D", "AWFOUT=<realAwfOutputDir>",
//	  "-f", "<profile.sb>",
//	  "--", "sh", "-c", <run>]
//
// rwDirs (credDirsWritable) is ignored on macOS: agents store credentials in
// the keychain (accessed via securityd, not a file write under $HOME), which
// survives the sandbox, so the Linux token-write-loss bug does not apply here.
// Granting the cred dirs write in the SBPL profile is a separate follow-up.
func (l sandboxExecLauncher) prepend(scratchDir string, _, _ []string) []string {
	// On macOS /var and /tmp are symlinks to /private/var and /private/tmp.
	// sandbox-exec evaluates paths through the VFS kernel layer, so the SBPL
	// (subpath ...) checks must use the real (symlink-resolved) path; using the
	// symlinked path causes false denials. filepath.EvalSymlinks resolves the
	// full chain; we fall back to filepath.Abs if the path does not yet exist.
	scratchAbs, err := filepath.Abs(scratchDir)
	if err != nil {
		scratchAbs = scratchDir
	}
	if realScratch, evalErr := filepath.EvalSymlinks(scratchAbs); evalErr == nil {
		scratchAbs = realScratch
	}
	rawTmpdir := os.TempDir()
	if l.tmpDirOverride != "" {
		rawTmpdir = l.tmpDirOverride
	}
	tmpdir, err := filepath.EvalSymlinks(rawTmpdir)
	if err != nil {
		tmpdir = rawTmpdir
	}

	// AWF_OUTPUT (spec §4.1) is the engine's typed-output capture path. Since U3,
	// native's AWF_OUTPUT lives at <workdir>/.awf/output (workdir-relative, per
	// Caps.OutputRoot), pre-created by Backend.Create — not container.AWFOutputDir
	// (docker/fake's container-private /tmp/awf). The workdir is already granted via
	// the SCRATCH param above, so this AWFOUT grant against container.AWFOutputDir is
	// vestigial for native; kept as a defensive extra allow, not the load-bearing
	// grant. Resolving symlinks keeps the SBPL subpath matching the real path the
	// kernel sees (macOS /tmp -> /private/tmp) even so.
	awfOut, err := filepath.EvalSymlinks(container.AWFOutputDir)
	if err != nil {
		if parent, perr := filepath.EvalSymlinks(filepath.Dir(container.AWFOutputDir)); perr == nil {
			awfOut = filepath.Join(parent, filepath.Base(container.AWFOutputDir))
		} else {
			awfOut = container.AWFOutputDir
		}
	}

	// Write the profile to a stable temp file. The file must outlive prepend
	// because sandbox-exec reads it after prepend returns.
	// v1 cleanup: leftover .sb in TMPDIR; acceptable, flagged in report.
	f, err := os.CreateTemp(tmpdir, "awf-sandbox-*.sb")
	if err != nil {
		// Fail CLOSED: returning nil would let exec.go run the step unsandboxed
		// on the host. Instead, return a sentinel argv that always exits non-zero
		// with a clear error message — consistent with the Linux trampoline's
		// fail-closed posture. Exit 125 is used (same sentinel as landlock).
		return failClosedArgv("awf: macOS sandbox profile could not be written; refusing to run step unsandboxed")
	}
	profilePath := f.Name()
	if _, werr := f.WriteString(sbplProfile); werr != nil {
		_ = f.Close()
		os.Remove(profilePath) //nolint:errcheck
		return failClosedArgv("awf: macOS sandbox profile could not be written; refusing to run step unsandboxed")
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(profilePath) //nolint:errcheck
		return failClosedArgv("awf: macOS sandbox profile could not be written; refusing to run step unsandboxed")
	}

	return []string{
		"sandbox-exec",
		"-D", "SCRATCH=" + scratchAbs,
		"-D", "TMPDIR=" + tmpdir,
		"-D", "AWFOUT=" + awfOut,
		"-f", profilePath,
		"--", "sh", "-c", l.run,
	}
}
