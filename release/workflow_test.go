package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// repoFile reads a path relative to the repository root. Tests in this package run
// with the working directory set to release/, so repo-root files are one level up.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

func requireContains(t *testing.T, where, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s: missing %q", where, want)
	}
}

func requireValidYAML(t *testing.T, name, body string) {
	t.Helper()
	var any any
	if err := yaml.Unmarshal([]byte(body), &any); err != nil {
		t.Fatalf("%s is not valid YAML: %v", name, err)
	}
}

// TestReleaseWorkflowContract locks the load-bearing invariants of the
// tag-triggered release workflow. If any of these silently drift, a tag push
// would either fail mid-publish or ship a malformed release.
func TestReleaseWorkflowContract(t *testing.T) {
	wf := repoFile(t, ".github/workflows/release.yml")
	requireValidYAML(t, "release.yml", wf)

	for _, want := range []string{
		// Trigger + identity.
		"name: Release",
		"tags:",
		"'v*'",
		// Least privilege: nothing by default, write scopes only on the release job.
		"permissions: {}",
		"contents: write",
		"id-token: write",
		"attestations: write",
		// One publish per tag.
		"concurrency:",
		// Strict SemVer gate (rejects v1, v1.2, v1.2.3-rc1).
		`^v[0-9]+\.[0-9]+\.[0-9]+$`,
		// Pinned actions.
		"actions/checkout@v7",
		"actions/setup-go@v6",
		"actions/upload-artifact@v7",
		"actions/download-artifact@v8",
		"actions/attest-build-provenance@v4",
		// The verify job reruns the same project gate as ci.yml.
		"make lint",
		"make test",
		"make build",
		"make integ",
		"vulnerable-service",
		// Four build targets on OS-matched runners.
		"goos: linux",
		"goos: darwin",
		"goarch: amd64",
		"goarch: arm64",
		"runner: ubuntu-latest",
		"runner: macos-15-intel",
		"runner: macos-15\n",
		"CGO_ENABLED",
		// Version stamp + the no-(devel) smoke guard.
		"-X github.com/valbaudo/awf/cli.version=",
		"(devel)",
		// Assemble + publish.
		"merge-multiple: true",
		"sha256sum",
		"gh release create",
		"--verify-tag",
		// Notes come from the curated CHANGELOG section, never auto-generated PR churn.
		"--notes-file",
		"CHANGELOG.md",
	} {
		requireContains(t, "release.yml", wf, want)
	}
}

// TestReleaseNotesConfigValid checks the auto-generated-release-notes config is
// present and parseable.
func TestReleaseNotesConfigValid(t *testing.T) {
	cfg := repoFile(t, ".github/release.yml")
	requireValidYAML(t, "release.yml (notes config)", cfg)
	requireContains(t, ".github/release.yml", cfg, "changelog:")
	requireContains(t, ".github/release.yml", cfg, "categories:")
}

// TestREADMEReleaseInstructions ensures the README documents the install and
// maintainer-release paths the workflow produces, so the docs and the artifacts
// can't drift apart.
func TestREADMEReleaseInstructions(t *testing.T) {
	readme := repoFile(t, "README.md")
	for _, want := range []string{
		"https://github.com/valbaudo/awf/releases",
		"awf_${VERSION}_${OS}_${ARCH}.tar.gz",
		"gh attestation verify",
		"go install github.com/valbaudo/awf/cmd/awf@",
		"git tag -s v0.1.0",
		"git push origin v0.1.0",
		"make lint test",
	} {
		requireContains(t, "README.md", readme, want)
	}
}
