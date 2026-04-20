package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// requireRunDir checks that <stateDir>/runs/<runID> exists. Returns ExitOK if
// present; ExitUsage with a helpful stderr message otherwise. Shared by the
// signal, pause, and cancel subcommands (rule-of-three: 3 identical call sites).
func requireRunDir(stateDir, runID string, stderr io.Writer) int {
	runDir := filepath.Join(stateDir, "runs", runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "no run with id %q at %q\n", runID, runDir)
		} else {
			fprintf(stderr, "stat run dir %q: %v\n", runDir, err)
		}
		return ExitUsage
	}
	return ExitOK
}
