.DEFAULT_GOAL := build
.PHONY: build test lint integ

build:
	go build -o bin/awf ./cmd/awf

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
integ:
	go test -race -tags=integ -count=1 -p 1 ./container/docker/... ./container/native/... ./cli/... ./conformance/...
