package claudesession

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// sessionUUID derives a deterministic UUID from the invocation's run identity
// and node path. The UUID is stable: same (RunID, CurrentEpoch, NodePath) →
// same UUID; different NodePath → different UUID.
//
// Algorithm: sha256("runID|epoch|nodePath"), first 16 bytes, formatted as a
// standard 8-4-4-4-12 UUID string (variant bits set per RFC 4122 §4.4 style
// but without version byte modification — the value is opaque to Claude Code,
// which only needs a stable unique string per session).
func sessionUUID(inv agent.AgentInvocation) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%d|%s",
		inv.RunContext.RunID,
		inv.RunContext.CurrentEpoch,
		inv.NodePath,
	)
	sum := h.Sum(nil) // 32 bytes
	b := sum[:16]
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// encodeProjectDir encodes a filesystem path as Claude Code's project-directory
// bucket name. Every `/`, `.`, and `_` is replaced with `-`. A path that starts
// with `/` therefore starts with `-` in the encoded form.
//
// Example: "/work/proj" → "-work-proj"
func encodeProjectDir(path string) string {
	r := strings.NewReplacer("/", "-", ".", "-", "_", "-")
	return r.Replace(path)
}

// SessionTranscriptPath implements agent.SessionPathProvider. It returns the
// path of the Claude Code session transcript file that the engine should
// capture after node.completed and restore before the next launch:
//
//	<homeDir>/.claude/projects/<encodeProjectDir(workdir)>/<sessionUUID(inv)>.jsonl
//
// homeDir is the Adapter's configured home directory (default: /root).
// workdir is the container working directory for the step.
func (a *Adapter) SessionTranscriptPath(inv agent.AgentInvocation, workdir string) string {
	uuid := sessionUUID(inv)
	enc := encodeProjectDir(workdir)
	return filepath.Join(a.homeDir, ".claude", "projects", enc, uuid+".jsonl")
}

// Compile-time assertion that Adapter satisfies agent.SessionPathProvider.
var _ agent.SessionPathProvider = (*Adapter)(nil)
