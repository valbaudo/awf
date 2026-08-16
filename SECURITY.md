# Security Policy

## Supported versions

`awf` is pre-1.0 and ships from a single line of development. Security fixes are
made against the most recent release; please upgrade to the latest `v0.x` before
reporting.

| Version       | Supported |
| ------------- | --------- |
| latest `v0.x` | ✅        |
| older         | ❌        |

The pre-1.0 caveat above is about the *binary*. AWF's machine-facing interfaces
carry a separate stability promise — see [COMPATIBILITY.md](COMPATIBILITY.md).

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

- Docker steps run in digest-pinned containers. The `native` backend attempts
  to write-confine host processes with bubblewrap or Landlock on Linux and
  `sandbox-exec` on macOS. With `AWF_SANDBOX_READS=open` (the default) reads are unrestricted and writes
  are confined; `confined` opts into deny-by-default reads (only enumerated
  system + agent-config dirs) for hosts that hold secrets worth denying reads
  to. It selects the first functionally usable launcher:
  on Linux, bubblewrap
  must successfully probe its namespace and mount policy before selection;
  otherwise AWF tries Landlock. If no launcher is usable, native retains a
  compatibility fallback that runs without confinement and prints a loud stderr
  warning. This fallback is **not fail-closed**. Use `--backend docker` when
  confinement is required.
- AWF never invokes `sudo`, `doas`, `pkexec`, or another privilege-elevation
  tool. State-mutating commands refuse those elevation provenances and require
  an existing state root to be owned by the invoking user. The check rejects
  provenance, not UID 0: genuine root and container sessions remain allowed
  when they own the state root. An elevated run can leave state that the normal
  user cannot safely update.
- `run:` step bodies and adapter prompts are shell/agent input **by design**.
  AWF does not sanitize workflow-authored commands — treat a workflow file as
  code you are choosing to run.
- AWF talks to a **local, user-controlled** container daemon. It does not expose
  a network service of its own.

## Known third-party advisories

`awf` vendors the Docker/Moby client toolchain to drive containers. As of
`v0.5.0`, `govulncheck` reports the following call-graph-reachable advisories in
that dependency chain. They are daemon- and build-host-class issues whose
exploitation presupposes a hostile or misconfigured Docker daemon or a hostile
image/build input — the daemon AWF talks to is the user's own — and several have
no upstream fix yet. We track upstream and bump as fixes land; Dependabot watches
these modules. (An earlier round cleared six advisories by bumping
`containerd/v2` → `v2.1.9` and `in-toto-golang` → `v0.11.0`; those are gone and
not listed here.)

| Advisory | Summary | Module(s) | Fixed upstream |
| --- | --- | --- | --- |
| [GO-2026-5746](https://pkg.go.dev/vuln/GO-2026-5746) | `PUT /containers/{id}/archive` executes container binary on the host | `github.com/docker/docker` | not yet |
| [GO-2026-5668](https://pkg.go.dev/vuln/GO-2026-5668) | `docker cp` race → arbitrary empty file creation via symlink swap | `github.com/docker/docker` | not yet |
| [GO-2026-5617](https://pkg.go.dev/vuln/GO-2026-5617) | `docker cp` race → bind-mount redirection to host path | `github.com/docker/docker` | not yet |
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) | Moby AuthZ plugin bypass on oversized request bodies | `github.com/docker/docker` | not yet |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) | Moby off-by-one in plugin privilege validation | `github.com/docker/docker` | not yet |
| [GO-2026-4859](https://pkg.go.dev/vuln/GO-2026-4859) | BuildKit Git-URL subdir → restricted-file access | `github.com/moby/buildkit` | `v0.28.1` |
| [GO-2026-4858](https://pkg.go.dev/vuln/GO-2026-4858) | BuildKit malicious frontend → file escape | `github.com/moby/buildkit` | `v0.28.1` |
| [GO-2026-4610](https://pkg.go.dev/vuln/GO-2026-4610) | Docker CLI plugin uncontrolled search path → local code execution | `github.com/docker/cli`, `github.com/docker/compose/v2` | `docker/cli v29.2.0`; compose not yet |
| [GO-2026-5378](https://pkg.go.dev/vuln/GO-2026-5378) | containerd user-ID handling bypass → `runAsNonRoot` evasion | `github.com/containerd/containerd/v2` | `v2.2.4` (deferred, see below) |

**Why these are still present.** The buildkit (GO-2026-4859, GO-2026-4858) and
docker/cli (GO-2026-4610) fixes require the Docker v29 toolchain, which has begun
relocating the client SDK from `github.com/docker/docker` to
`github.com/moby/moby`. The packages AWF uses to drive Compose —
`docker/compose/v2` (latest v2.40.3) and `docker/buildx` (v0.29.1) — still use the
old module and fail to compile against the patched buildkit/cli, and no compatible
release exists yet (re-verified 2026-07-08: bumping buildkit to v0.28.1 pulls
`docker/cli v29.2.1`, which then requires the `github.com/moby/moby/client` SDK
that Compose has not adopted). We are waiting on a `docker/compose/v2` release
that adopts the v29 stack; Dependabot will re-propose the bump once it builds. The
five `github.com/docker/docker` advisories have no upstream fix in any version.
GO-2026-5378 is fixed in `containerd/v2 v2.2.4`, but that bump transitively drags
`k8s.io/{api,apimachinery,client-go}` from `v0.32` to `v0.34` (AWF calls none of
it directly); it is a CRI-runtime user-ID/`runAsNonRoot` class issue outside how
AWF uses containerd, so the bump is deferred until it can ride a wider
containerd/compose update.

Re-check at any time with [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck):

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
