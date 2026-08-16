//go:build integ && live

package codex_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live contract probe against the REAL codex binary (runs under
// `make integ-live`, skips cleanly without a codex on PATH).
//
// Why this exists (2026-08-16 RCA): the adapter's CLI assumptions were
// hand-verified once per adoption and rotted twice in two days — the effort
// enum froze at v0.131.0 (codex later added max/ultra), and "OPENAI_API_KEY in
// env authenticates exec" was never true on current codex (it needs auth.json
// via `codex login --with-api-key`, and codex refuses to create CODEX_HOME
// itself). Each probe below machine-checks one load-bearing assumption; a
// failure means the adapter's contract drifted, with the failure message
// naming the assumption. Two deliberately-unauthenticated API calls (401s —
// no spend).
//
// Run when touching agent/codex or adopting a new pinned codex version.

func probeBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex binary not on PATH")
	}
	return path
}

func probeExec(t *testing.T, codexHome string, withKey bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probeBin(t), "exec", "--json", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only", "--", "probe")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CODEX_HOME=" + codexHome}
	if withKey {
		env = append(env, "OPENAI_API_KEY=sk-probe-deliberately-invalid")
	}
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func TestProbeCodexVersionHandshake(t *testing.T) {
	cmd := exec.Command(probeBin(t), "--version")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "codex") {
		t.Fatalf("ASSUMPTION BROKEN: `codex --version` — the adapter's version handshake: %v (%s)", err, out)
	}
}

func TestProbeCodexAuthContract(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	// A1: codex refuses a NONEXISTENT CODEX_HOME (the adapter's login prelude
	// mkdir -p's first — 2026-08-16, "points to … but that path does not exist").
	envOnly := probeExec(t, codexHome, true)
	if !strings.Contains(envOnly, "does not exist") && !strings.Contains(envOnly, "401") {
		t.Fatalf("ASSUMPTION BROKEN: codex accepted a nonexistent CODEX_HOME (or failed differently): %s", envOnly)
	}

	// A2: env-only key with an EMPTY-but-existing home → auth failure. Current
	// contract: "Missing bearer" (the env var is IGNORED — the login prelude is
	// required). If a future codex honors env auth again this leg fails loudly
	// and the prelude can be retired.
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	out := probeExec(t, codexHome, true)
	if !strings.Contains(out, "401") && !strings.Contains(out, "Unauthorized") {
		t.Fatalf("ASSUMPTION BROKEN: env-only invalid key did not produce an auth failure: %s", out)
	}
	t.Logf("env-only auth failure mode: %s", firstLineContaining(out, "401"))

	// A3: login --with-api-key materializes auth.json OFFLINE (no online
	// validation of the key), and afterwards the bearer IS sent — the failure
	// becomes "key rejected" (401 invalid), never "Missing bearer". This is the
	// adapter prelude's core assumption.
	login := exec.Command(probeBin(t), "login", "--with-api-key")
	login.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CODEX_HOME=" + codexHome}
	login.Stdin = strings.NewReader("sk-probe-deliberately-invalid")
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("ASSUMPTION BROKEN: `codex login --with-api-key` validates online or changed shape: %v (%s)", err, out)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatalf("ASSUMPTION BROKEN: login did not write auth.json: %v", err)
	}
	out = probeExec(t, codexHome, false)
	if strings.Contains(out, "Missing bearer") {
		t.Fatalf("ASSUMPTION BROKEN: bearer not sent after auth.json materialization: %s", out)
	}
	if !strings.Contains(out, "401") && !strings.Contains(out, "Unauthorized") && !strings.Contains(out, "nvalid") {
		t.Fatalf("ASSUMPTION BROKEN: expected invalid-key rejection after login, got: %s", out)
	}
}

func firstLineContaining(s, needle string) string {
	for line := range strings.Lines(s) {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return "(none)"
}
