# Security Policy

## Supported versions

`awf` is pre-1.0 and ships from a single line of development. Security fixes are
made against the most recent release; please upgrade to the latest `v0.x` before
reporting.

| Version       | Supported |
| ------------- | --------- |
| latest `v0.x` | ✅        |
| older         | ❌        |

## Reporting a vulnerability

Report security issues **privately** through GitHub's
[private vulnerability reporting](https://github.com/valbaudo/awf/security/advisories/new)
— the "Report a vulnerability" button on the repository's **Security** tab.
Please do not open a public issue for a suspected vulnerability.

Include enough detail to reproduce: the workflow, the backend (`native` or
`docker`), the adapter, and your `awf version` output. We aim to acknowledge a
report within a few days.

## Threat model notes

`awf` orchestrates **untrusted agent and command output**. A few properties are
intentional and worth understanding before reporting:

- Steps run in a sandbox appropriate to the backend: digest-pinned containers
  (`docker`), or host processes write-confined by an OS sandbox (`native` —
  bubblewrap or Landlock on Linux, `sandbox-exec` on macOS). The `native`
  sandbox is **fail-closed**: a step refuses to run if no sandbox mechanism is
  available.
- `run:` step bodies and adapter prompts are shell/agent input **by design**.
  AWF does not sanitize workflow-authored commands — treat a workflow file as
  code you are choosing to run.
- AWF talks to a **local, user-controlled** container daemon. It does not expose
  a network service of its own.

## Known third-party advisories

`awf` vendors the Docker/Moby client toolchain to drive containers. As of
`v0.1.0`, `govulncheck` reports the following call-graph-reachable advisories in
that dependency chain. They are daemon- and build-host-class issues whose
exploitation presupposes a hostile or misconfigured Docker daemon — the daemon
AWF talks to is the user's own — and several have no upstream fix yet. We track
upstream and bump as fixes land; Dependabot watches these modules.

| Advisory | Summary | Module(s) | Fixed upstream |
| --- | --- | --- | --- |
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) | Moby AuthZ plugin bypass on oversized request bodies | `github.com/docker/docker` | not yet |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) | Moby off-by-one in plugin privilege validation | `github.com/docker/docker` | not yet |
| [GO-2026-4859](https://pkg.go.dev/vuln/GO-2026-4859) | BuildKit Git-URL subdir → restricted-file access | `github.com/moby/buildkit` | `v0.28.1` |
| [GO-2026-4858](https://pkg.go.dev/vuln/GO-2026-4858) | BuildKit malicious frontend → file escape | `github.com/moby/buildkit` | `v0.28.1` |
| [GO-2026-4610](https://pkg.go.dev/vuln/GO-2026-4610) | Docker CLI plugin uncontrolled search path → local code execution | `github.com/docker/cli`, `github.com/docker/compose/v2` | `docker/cli v29.2.0`; compose not yet |

**Why these are still present.** The buildkit (GO-2026-4859, GO-2026-4858) and
docker/cli (GO-2026-4610) fixes require the Docker v29 toolchain, which has begun
relocating the client SDK from `github.com/docker/docker` to
`github.com/moby/moby`. The packages AWF uses to drive Compose —
`docker/compose/v2` (latest v2.40.3) and `docker/buildx` (v0.29.1) — still use the
old module and fail to compile against the patched buildkit/cli, and no compatible
release exists yet (verified 2026-06-24: bumping buildkit to v0.28.1 breaks
`docker/buildx` on `docker/docker` ↔ `moby/moby` type mismatches). We are waiting
on a `docker/compose/v2` release that adopts the v29 stack; Dependabot will
re-propose the bump once it builds. The two `github.com/docker/docker` advisories
have no upstream fix in any version.

Re-check at any time with [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck):

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
