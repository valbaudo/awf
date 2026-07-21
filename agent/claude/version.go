package claude

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/valbaudo/awf/container"
)

// versionCommand is the shell command Version runs. Constant so tests can
// match it deterministically via Fake.ProgramExec.
const versionCommand = "claude --version"

// semverPattern matches a leading `\d+\.\d+\.\d+(-\w+)?` token at the start
// of the output. Phase 5 design § C: tolerant of trailing "(Claude Code)"
// suffix and trailing build-hash; missing-semver → the first non-empty
// line literal.
var semverPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+(?:-[\w.]+)?)`)

// Version runs `claude --version` inside the supplied handle and parses
// the leading semver token. Per Phase 5 design decision 5, version IS
// per-container — the binary's PATH is per-container. Called once per
// (uses, container) pair at run start; recorded in run.started.Runtimes;
// re-resolved on resume; drift hard-errors (cli/resume.go).
func (a *Adapter) Version(ctx context.Context, handle container.Handle) (string, error) {
	if a.backend == nil {
		return "", fmt.Errorf("agent/claude: Version: no Backend wired (use WithBackend in New)")
	}
	chunks, result, err := a.backend.Exec(ctx, handle, container.Cmd{Run: versionCommand, AgentRuntime: true})
	if err != nil {
		return "", &ErrAgentRuntimeNotFound{
			Ref:       AdapterRef,
			Container: handle.Name,
			Cause:     err,
		}
	}
	// Drain the stream (Version output is small; one chunk typical).
	for range chunks {
	}
	r := <-result
	if r.ExitCode != 0 {
		return "", &ErrAgentRuntimeNotFound{
			Ref:       AdapterRef,
			Container: handle.Name,
			Cause:     fmt.Errorf("claude --version exited with code %d", r.ExitCode),
		}
	}
	first := firstNonEmptyLine(r.Stdout)
	if first == "" {
		return "", &ErrAgentRuntimeNotFound{
			Ref:       AdapterRef,
			Container: handle.Name,
			Cause:     fmt.Errorf("claude --version produced no output"),
		}
	}
	if m := semverPattern.FindStringSubmatch(first); len(m) > 1 {
		return m[1], nil
	}
	return first, nil
}

func firstNonEmptyLine(b []byte) string {
	s := string(b)
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
