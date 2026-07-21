package codex

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/valbaudo/awf/container"
)

const versionCommand = "codex --version"

// semverPattern is UN-anchored (FindStringSubmatch scans the whole line): codex
// prints "codex-cli 0.131.0", so goose's `^`-anchored pattern would miss. COPIED
// (not shared) per the COPY-don't-share rule — agent/ exports no semverPattern.
var semverPattern = regexp.MustCompile(`(\d+\.\d+\.\d+(?:-[\w.]+)?)`)

// Version runs `codex --version` inside handle and parses the leading semver.
// Recorded in run.started.Runtimes; re-resolved on resume; drift hard-errors.
func (a *Adapter) Version(ctx context.Context, handle container.Handle) (string, error) {
	if a.backend == nil {
		return "", fmt.Errorf("agent/codex: Version: no Backend wired (use WithBackend in New)")
	}
	chunks, result, err := a.backend.Exec(ctx, handle, container.Cmd{Run: versionCommand, AgentRuntime: true})
	if err != nil {
		return "", &ErrRuntimeNotFound{Ref: AdapterRef, Container: handle.Name, Cause: err}
	}
	for range chunks { // drain before reading result (deadlock otherwise)
	}
	r := <-result
	if r.ExitCode != 0 {
		return "", &ErrRuntimeNotFound{Ref: AdapterRef, Container: handle.Name, Cause: fmt.Errorf("codex --version exited with code %d", r.ExitCode)}
	}
	first := firstNonEmptyLine(r.Stdout)
	if first == "" {
		return "", &ErrRuntimeNotFound{Ref: AdapterRef, Container: handle.Name, Cause: fmt.Errorf("codex --version produced no output")}
	}
	if m := semverPattern.FindStringSubmatch(first); len(m) > 1 {
		return m[1], nil
	}
	return first, nil
}

// firstNonEmptyLine returns the first trimmed non-empty line of b. Also used by
// launch.go's diagnostic path. COPIED verbatim from agent/goose (package-private).
func firstNonEmptyLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
