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
# Docker daemon (DOCKER_HOST / unix socket). Phase 4 slice 4.1 ships the
# first integ tests (Bucket 9a — Docker image-mode); later slices add
# Buckets 9b/9c/10/11 to the same ./container/docker/... target.
#
# Target is narrow on purpose: only container/docker/ ships integ-tagged
# tests in Phase 4. Using ./... here would re-compile every other package
# with the integ tag for no benefit. If a future slice adds integ tests
# elsewhere (e.g., a cli/ integration test), broaden this target then.
#
# -count=1 disables Go's test caching (integ tests have hidden inputs like
# daemon state). -race stays on; if a real race surfaces in the Docker SDK,
# document the specific symptom + SDK version then.
integ:
	go test -race -tags=integ -count=1 ./container/docker/...
