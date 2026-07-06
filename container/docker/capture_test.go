package docker

import "testing"

// TestResolveCapturePath is the pure-function unit test for F8: a relative
// output_files.path must resolve against the container's WORKDIR (not "/",
// which is what docker's CopyFromContainer does with a relative archive
// path by default); an absolute path passes through unchanged. No docker
// required — runs under default `go test` / `make test`.
func TestResolveCapturePath(t *testing.T) {
	if got := resolveCapturePath("/app", "out/x.json"); got != "/app/out/x.json" {
		t.Fatalf("relative should join WORKDIR: %q", got)
	}
	if got := resolveCapturePath("/app", "/tmp/x.json"); got != "/tmp/x.json" {
		t.Fatalf("absolute unchanged: %q", got)
	}
}
