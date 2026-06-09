# Vulnerability Triage - 2026-06-09

Scope: `govulncheck ./...` after the minimal dependency refresh for the AWF
subworkflow preflight. The refresh updates the Go toolchain to 1.26.4 for
reachable standard-library findings and updates the compatible `x/crypto` and
`moby/spdystream` fixed lines. The scan still reports reachable Docker/Moby
findings with no fixed version and Docker/BuildKit fixed-version findings whose
available fixes require a broader Docker CLI/Compose migration.

## Reachable Fixed:N/A Findings

| ID | Module | Found in | Fixed in | Reachability | Risk | Mitigation | Follow-up |
| --- | --- | --- | --- | --- | --- | --- | --- |
| GO-2026-4887 | `github.com/docker/docker` | `v28.5.2+incompatible` | N/A | Reachable through Docker backend client initialization and container API calls. | AuthZ plugin bypass for oversized request bodies in Moby. AWF is a Docker API client, not a Docker daemon or AuthZ plugin host. | Keep AWF as a local single-host client; do not expose AWF as a network-facing Docker API proxy. Rely on host Docker daemon patching/configuration for daemon-side AuthZ enforcement. | Re-run `govulncheck ./...` when Moby publishes a fixed module version or AWF migrates to the split Moby API/client modules. |
| GO-2026-4883 | `github.com/docker/docker` | `v28.5.2+incompatible` | N/A | Reachable through Docker backend client initialization and container API calls. | Moby plugin privilege validation off-by-one. AWF does not install or validate Docker plugins. | Keep plugin installation outside AWF runtime scope; rely on host Docker daemon patching/configuration for plugin privilege validation. | Re-run `govulncheck ./...` when Moby publishes a fixed module version or AWF migrates to the split Moby API/client modules. |
| GO-2026-4610 | `github.com/docker/compose/v2` | `v2.40.3` | N/A | Reachable through Compose service setup used by the Docker backend. | Docker CLI plugin search-path issue on Windows. AWF resolves and invokes Compose as a library path on the local host; the project is single-host and currently developed/tested on Unix-like environments. | Treat Windows execution as not cleared by this triage until Docker Compose publishes a fixed module or AWF changes its Docker/Compose integration. | Re-run `govulncheck ./...` after a Compose release that supports the fixed Docker CLI module line. |

## Remaining Fixed-Version Findings

These are not triaged as Fixed:N/A. They remain blockers for a clean
`govulncheck` result, but the fixed versions currently require a broader Docker
module migration:

| ID | Module | Found in | Fixed in | Current blocker |
| --- | --- | --- | --- | --- |
| GO-2026-4858 | `github.com/moby/buildkit` | `v0.25.1` | `v0.28.1` | `github.com/moby/buildkit@v0.28.1` requires `github.com/docker/cli@v29.2.1+incompatible`, which is incompatible with released `github.com/docker/compose/v2@v2.40.3` package loading. |
| GO-2026-4859 | `github.com/moby/buildkit` | `v0.25.1` | `v0.28.1` | Same Docker CLI / released Compose compatibility blocker as GO-2026-4858. |
| GO-2026-4610 | `github.com/docker/cli` | `v28.5.1+incompatible` | `v29.2.0+incompatible` | Updating Docker CLI to the fixed line breaks package loading with released `github.com/docker/compose/v2@v2.40.3`; no newer Compose release is listed by `go list -m -versions github.com/docker/compose/v2` in this environment. |
