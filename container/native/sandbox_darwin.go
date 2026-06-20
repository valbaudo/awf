//go:build darwin

package native

import (
	"os"
	"path/filepath"
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

// prepend is a sentinel no-op. exec.go always calls buildForRun first.
func (f *sandboxExecFactory) prepend(_ string, _ []string) []string { return nil }

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
//	  "-f", "<profile.sb>",
//	  "--", "sh", "-c", <run>]
func (l sandboxExecLauncher) prepend(scratchDir string, _ []string) []string {
	// On macOS /var and /tmp are symlinks to /private/var and /private/tmp.
	// sandbox-exec evaluates paths through the VFS kernel layer, so the SBPL
	// (subpath ...) checks must use the real (symlink-resolved) path; using the
	// symlinked path causes false denials. filepath.EvalSymlinks resolves the
	// full chain; we fall back to filepath.Abs if the path does not yet exist.
	scratchAbs, err := filepath.EvalSymlinks(scratchDir)
	if err != nil {
		scratchAbs, err = filepath.Abs(scratchDir)
		if err != nil {
			scratchAbs = scratchDir
		}
	}
	rawTmpdir := os.TempDir()
	tmpdir, err := filepath.EvalSymlinks(rawTmpdir)
	if err != nil {
		tmpdir = rawTmpdir
	}

	// Write the profile to a stable temp file. The file must outlive prepend
	// because sandbox-exec reads it after prepend returns.
	// v1 cleanup: leftover .sb in TMPDIR; acceptable, flagged in report.
	f, err := os.CreateTemp(tmpdir, "awf-sandbox-*.sb")
	if err != nil {
		// Fail closed: return nil so exec.go runs sh directly (no confinement).
		// This is an OS-level failure (disk full, permission denied on TMPDIR).
		return nil
	}
	profilePath := f.Name()
	if _, werr := f.WriteString(sbplProfile); werr != nil {
		_ = f.Close()
		os.Remove(profilePath) //nolint:errcheck
		return nil
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(profilePath) //nolint:errcheck
		return nil
	}

	return []string{
		"sandbox-exec",
		"-D", "SCRATCH=" + scratchAbs,
		"-D", "TMPDIR=" + tmpdir,
		"-f", profilePath,
		"--", "sh", "-c", l.run,
	}
}
