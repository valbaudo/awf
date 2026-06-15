package cli

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// TestResolveVersionExtractsVCSSettings checks that the VCS metadata baked into a real
// `go build` (vcs.revision/time/modified) is lifted out of debug.BuildInfo verbatim — full
// commit sha (not truncated), the build time, and the dirty flag.
func TestResolveVersionExtractsVCSSettings(t *testing.T) {
	bi := &debug.BuildInfo{
		GoVersion: "go1.99",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
			{Key: "vcs.time", Value: "2026-06-15T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	got := resolveVersion("v1.2.3", "go1.99", bi, true)
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got.Version)
	}
	if got.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("Commit = %q, want full sha (JSON carries the untruncated revision)", got.Commit)
	}
	if got.BuildTime != "2026-06-15T00:00:00Z" {
		t.Errorf("BuildTime = %q", got.BuildTime)
	}
	if !got.Dirty {
		t.Errorf("Dirty = false, want true (vcs.modified=true)")
	}
	if got.GoVersion != "go1.99" {
		t.Errorf("GoVersion = %q", got.GoVersion)
	}
}

// TestResolveVersionNoBuildInfo: a `go run` binary (ok=false / no VCS keys) must never error —
// the VCS fields stay empty while version + go version survive.
func TestResolveVersionNoBuildInfo(t *testing.T) {
	got := resolveVersion("(devel)", "go1.99", nil, false)
	if got.Commit != "" || got.BuildTime != "" || got.Dirty {
		t.Errorf("expected empty VCS fields for no-buildinfo, got %+v", got)
	}
	if got.Version != "(devel)" || got.GoVersion != "go1.99" {
		t.Errorf("Version/GoVersion lost: %+v", got)
	}
}

// TestVersionTextFull locks the one-line text shape: 12-char sha, +dirty suffix, build time.
func TestVersionTextFull(t *testing.T) {
	v := versionInfo{Version: "v1.2.3", Commit: "0123456789abcdef0123456789abcdef01234567", Dirty: true, BuildTime: "2026-06-15T00:00:00Z", GoVersion: "go1.99"}
	want := "awf v1.2.3 (commit 0123456789ab+dirty, built 2026-06-15T00:00:00Z, go1.99)"
	if got := v.text(); got != want {
		t.Errorf("text() = %q, want %q", got, want)
	}
}

// TestVersionTextCleanNoDirty: a clean tree drops the +dirty suffix.
func TestVersionTextCleanNoDirty(t *testing.T) {
	v := versionInfo{Version: "v1.2.3", Commit: "0123456789abcdef", BuildTime: "2026-06-15T00:00:00Z", GoVersion: "go1.99"}
	want := "awf v1.2.3 (commit 0123456789ab, built 2026-06-15T00:00:00Z, go1.99)"
	if got := v.text(); got != want {
		t.Errorf("text() = %q, want %q", got, want)
	}
}

// TestVersionTextFallbackNoCommit: with no commit (go run), collapse to the "commit unknown"
// form and omit the build time — never a half-populated line.
func TestVersionTextFallbackNoCommit(t *testing.T) {
	v := versionInfo{Version: "(devel)", GoVersion: "go1.99"}
	want := "awf (devel) (commit unknown, go1.99)"
	if got := v.text(); got != want {
		t.Errorf("text() = %q, want %q", got, want)
	}
}

// TestRunVersionSubcommandText: `awf version` exits 0 and prints a single "awf ..." line.
func TestRunVersionSubcommandText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "awf ") {
		t.Errorf("version text = %q, want prefix %q", stdout.String(), "awf ")
	}
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Errorf("version text should be one line, got %q", stdout.String())
	}
}

// TestRunVersionFlagSameAsSubcommand: `awf --version` prints byte-identical output to
// `awf version` (the spec's "both print the same string").
func TestRunVersionFlagSameAsSubcommand(t *testing.T) {
	var a, b bytes.Buffer
	Run([]string{"version"}, &a, new(bytes.Buffer))
	Run([]string{"--version"}, &b, new(bytes.Buffer))
	if a.String() != b.String() {
		t.Errorf("`version` = %q, `--version` = %q; want identical", a.String(), b.String())
	}
}

// TestRunVersionJSON: `awf version -o json` emits valid JSON with the five contract keys
// (empty strings where unknown — so the keys are always present, never omitempty).
func TestRunVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version", "-o", "json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("version -o json not valid JSON: %v\n%s", err, stdout.String())
	}
	for _, k := range []string{"version", "commit", "dirty", "build_time", "go_version"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %q in %s", k, stdout.String())
		}
	}
}
