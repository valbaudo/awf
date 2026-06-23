// Package release holds repository-contract tests that guard the release
// pipeline: the tag-triggered GitHub Actions workflow and the README's install
// and maintainer-release instructions. It has no runtime code.
//
// A tag-only workflow is never exercised until the moment it must work — the
// first push of a version tag. These tests run under `make test`, so a broken
// release.yml (invalid YAML, a dropped SemVer guard, a missing artifact name)
// fails the gate on an ordinary PR instead of at the worst possible time.
package release
