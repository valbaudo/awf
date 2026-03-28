.DEFAULT_GOAL := build
.PHONY: build test lint

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
