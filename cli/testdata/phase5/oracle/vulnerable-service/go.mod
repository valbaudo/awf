// Sub-module — intentionally excluded from `go test ./...` from the
// repo root so the main test suite stays fast. CI runs this module's
// tests via the dedicated step in .github/workflows/ci.yml (slice 5.4
// Task 12). Keep the Go version aligned with the root go.mod (1.26).

module vulnerable-service

go 1.26
