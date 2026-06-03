.DEFAULT_GOAL := build
.PHONY: build test lint integ integ-live man

build:
	go build -o bin/awf ./cmd/awf

# man builds the troff man pages from their Markdown sources via go-md2man.
# go-md2man is pinned (the version already in the module graph) so output is
# reproducible, and is run via `go run` — no system dependency and no go.mod
# entry, since it is a build tool rather than a library import. Sources
# (man/*.md) are committed; the generated man/awf.1 and man/awf-workflow.5 are
# gitignored build artifacts. View locally with e.g. `man ./man/awf.1`.
MD2MAN := go run github.com/cpuguy83/go-md2man/v2@v2.0.7

man: man/awf.1 man/awf-workflow.5

man/%: man/%.md
	$(MD2MAN) -in $< -out $@

test:
	go test -race ./...

lint:
	@UNFORMATTED=$$(gofmt -l .); \
	if [ -n "$$UNFORMATTED" ]; then \
		echo "gofmt: the following files are not formatted:" >&2; \
		echo "$$UNFORMATTED" >&2; \
		exit 1; \
	fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found; skipping (CI runs it)" >&2; \
	fi

# integ runs the Docker-required integration tests. Requires a working
# Docker daemon (DOCKER_HOST / unix socket). Phase 4 slice 4.1 shipped the
# first integ tests (Bucket 9a — Docker image-mode); slices 4.2–4.4 added
# Buckets 9b/9c/10/11 under ./container/docker/.... Slice 4.5 added
# ./cli/... integ tests (CVE-pipeline boots-under-Docker + pause-resume
# round-trip via the CLI --backend flag). Slice 4.6 added
# ./conformance/... integ tests (TestConformanceDockerBackend, which
# replays Buckets 9/10/11 through the shared conformance suite). Slice 4.7
# added ./container/native/... integ tests (TestNativeRunBasicContract,
# conformance bar for the native process backend).
#
# Target is narrow on purpose: only container/docker/, container/native/,
# cli/, and conformance/ ship integ-tagged tests today. Using ./... here
# would re-compile every other package with the integ tag for no benefit.
# If a future slice adds integ tests elsewhere, append that package to this
# list then.
#
# -count=1 disables Go's test caching (integ tests have hidden inputs like
# daemon state). -race stays on; if a real race surfaces in the Docker SDK,
# document the specific symptom + SDK version then.
#
# COST: `integ` spends NO API money. It builds with -tags=integ only, which
# EXCLUDES every test carrying the extra `live` constraint (the real-`claude`
# tier — see `integ-live` below). What runs here is Docker/native exec
# plumbing + compose lifecycle + the fake-backed conformance buckets: all
# free. This is the per-PR CI target; .github/workflows/ci.yml runs it.
integ:
	go test -race -tags=integ -count=1 -p 1 -timeout=30m ./container/docker/... ./container/native/... ./cli/... ./conformance/...

# integ-live runs the real-`claude` tier — the handful of tests that exec the
# actual claude CLI and may spend Anthropic API money. It is LOCAL-ONLY and
# deliberately NOT referenced by any CI workflow.
#
# Why local-only: AWF pins the resolved runtime version and hard-errors on
# drift (standard §8). It does not monitor what Anthropic ships next — a
# newer claude is a version you have not adopted, not a regression to catch.
# So these tests have exactly two trigger conditions: you changed the
# agent/claude adapter, or you are deliberately bumping the pinned claude
# version (re-capturing the testdata/*.jsonl cassettes). Outside those, real
# Claude is already proven and spending money tells you nothing about AWF.
#
# The `live` tag is additive over `integ` (-tags='integ live'), so this also
# pulls in the free Docker/conformance tests; that is fine for a local run.
# Each live test t.Skips cleanly when `claude` is absent or no auth env var
# is set, so even `integ-live` is free until you actually have credentials.
integ-live:
	go test -race -tags='integ live' -count=1 -p 1 -timeout=30m ./agent/claude/... ./agent/droid/... ./agent/goose/... ./conformance/... ./cli/...
