//go:build integ

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

const nonRootFixtureUID = "65532"

func TestNonRootWorkspaceOwnershipAcrossDockerOperations(t *testing.T) {
	cli, b := newTestBackend(t, "nonroot-ownership")
	ctx := context.Background()
	imageRef := buildNonRootFixture(t, ctx, cli)

	h, err := b.Create(ctx, cont.ContainerSpec{Name: "nonroot", Image: imageRef})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			_ = b.Destroy(ctx, h)
		}
	})
	assertContainerUID(t, ctx, b, h, nonRootFixtureUID)
	execOK(t, ctx, b, h, `test "$(stat -c %u:%g:%a /work)" = "65532:65532:755"`)
	execOK(t, ctx, b, h, `test -w /work/.awf && test -w /tmp/awf`)

	if err := b.CopyTo(ctx, h, []cont.InputFile{
		{Path: "/work/staged/append.txt", Content: []byte("seed")},
		{Path: "/work/staged/overwrite.txt", Content: []byte("old")},
	}); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	execOK(t, ctx, b, h, `printf '+append' >> /work/staged/append.txt && printf 'new' > /work/staged/overwrite.txt`)
	assertContainerUID(t, ctx, b, h, nonRootFixtureUID)

	if err := b.WriteFileAt(ctx, h, "/work/file-at.txt", []byte("file")); err != nil {
		t.Fatalf("WriteFileAt: %v", err)
	}
	execOK(t, ctx, b, h, `printf '+append' >> /work/file-at.txt`)
	assertContainerUID(t, ctx, b, h, nonRootFixtureUID)

	tree, err := cont.BuildTreeTar(map[string][]byte{"nested/value.txt": []byte("tree")})
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}
	if err := b.WriteTreeAt(ctx, h, "/work/tree", tree); err != nil {
		t.Fatalf("WriteTreeAt: %v", err)
	}
	execOK(t, ctx, b, h, `printf '+mutated' >> /work/tree/nested/value.txt && printf 'sibling' > /work/tree/nested/sibling.txt`)
	assertContainerUID(t, ctx, b, h, nonRootFixtureUID)

	// No staged inputs and no author mkdir: Backend.Create must have prepared
	// /tmp/awf with ownership matching the configured non-root image user.
	d := &engine.LocalDispatcher{Backend: b, Handles: map[string]cont.Handle{"lab": h}}
	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"owned"},
		"properties":           map[string]any{"owned": map[string]any{"type": "boolean"}},
	}
	result, chunks, err := d.Run(ctx, engine.NodeIntent{
		Path: "typed-output",
		Node: &ir.CodeStep{ID: "typed-output", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      `printf '{"owned":true}\n' > "$AWF_OUTPUT"`,
			OutputSchema: &schema,
		},
	})
	if err != nil {
		t.Fatalf("LocalDispatcher.Run: %v", err)
	}
	for range chunks {
	}
	if result.Outcome != engine.OutcomeOK || result.Outputs["owned"] != true {
		t.Fatalf("typed output result = outcome %s outputs=%v err=%v", result.Outcome, result.Outputs, result.Err)
	}
	assertContainerUID(t, ctx, b, h, nonRootFixtureUID)

	ref, err := b.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy before Restore: %v", err)
	}
	destroyed = true

	restored, err := b.Restore(ctx, ref, "restored")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, restored) })
	assertContainerUID(t, ctx, b, restored, nonRootFixtureUID)
	execOK(t, ctx, b, restored, `printf '+restored' >> /work/tree/nested/value.txt && printf 'new' > /work/restored-sibling.txt && test -w /work/.awf && test -w /tmp/awf`)
}

func TestNonRootComposePreparesEveryService(t *testing.T) {
	cli, b := newTestBackend(t, "nonroot-compose")
	ctx := context.Background()
	imageRef := buildNonRootFixture(t, ctx, cli)
	compose := []byte(fmt.Sprintf(`services:
  web:
    image: %s
  worker:
    image: %s
`, imageRef, imageRef))
	h, err := b.Create(ctx, cont.ContainerSpec{
		Name:        "lab",
		Compose:     compose,
		ComposePath: "nonroot-compose.yml",
		Service:     "web",
	})
	if err != nil {
		t.Fatalf("Create compose: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	for _, service := range []string{"web", "worker"} {
		serviceHandle := h
		serviceHandle.Service = service
		assertContainerUID(t, ctx, b, serviceHandle, nonRootFixtureUID)
		execOK(t, ctx, b, serviceHandle, `test -w /work/.awf && test -w /tmp/awf && printf ok > /work/.awf/`+service)
	}
}

func TestRootImageRuntimeDirectoriesRemainWritable(t *testing.T) {
	cli, b := newTestBackend(t, "root-runtime-dirs")
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull Alpine: %v", err)
	}
	h, err := b.Create(ctx, cont.ContainerSpec{Name: "root", Image: alpineDigest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	assertContainerUID(t, ctx, b, h, "0")
	execOK(t, ctx, b, h, `test -w /work/.awf && test -w /tmp/awf && printf ok > /work/.awf/root.txt`)
}

func buildNonRootFixture(t *testing.T, ctx context.Context, cli *client.Client) string {
	t.Helper()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull pinned Alpine: %v", err)
	}
	tag := fmt.Sprintf("awf-nonroot-fixture:%d", time.Now().UnixNano())
	dockerfile := fmt.Sprintf(`FROM %s
RUN mkdir -p /work && chown 65532:65532 /work && chmod 0755 /work
USER 65532:65532
WORKDIR /work
CMD ["sleep", "infinity"]
`, alpineDigest)
	var contextTar bytes.Buffer
	tw := tar.NewWriter(&contextTar)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("Dockerfile header: %v", err)
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		t.Fatalf("Dockerfile body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("build context close: %v", err)
	}
	response, err := cli.ImageBuild(ctx, bytes.NewReader(contextTar.Bytes()), build.ImageBuildOptions{
		Dockerfile: "Dockerfile",
		Tags:       []string{tag},
		Remove:     true,
	})
	if err != nil {
		t.Fatalf("ImageBuild: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	dec := json.NewDecoder(response.Body)
	for {
		var event struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode ImageBuild output: %v", err)
		}
		if event.ErrorDetail.Message != "" {
			t.Fatalf("ImageBuild: %s", event.ErrorDetail.Message)
		}
		if event.Error != "" {
			t.Fatalf("ImageBuild: %s", event.Error)
		}
	}
	t.Cleanup(func() {
		if _, err := cli.ImageRemove(context.Background(), tag, image.RemoveOptions{Force: true}); err != nil {
			t.Logf("remove fixture image %s: %v", tag, err)
		}
	})
	return tag
}

func assertContainerUID(t *testing.T, ctx context.Context, b *Backend, h cont.Handle, want string) {
	t.Helper()
	got := strings.TrimSpace(execOutput(t, ctx, b, h, `id -u`))
	if got != want {
		t.Fatalf("id -u = %q, want %q", got, want)
	}
}

func execOK(t *testing.T, ctx context.Context, b *Backend, h cont.Handle, command string) {
	t.Helper()
	_ = execOutput(t, ctx, b, h, command)
}

func execOutput(t *testing.T, ctx context.Context, b *Backend, h cont.Handle, command string) string {
	t.Helper()
	chunks, resultCh, err := b.Exec(ctx, h, cont.Cmd{Run: command})
	if err != nil {
		t.Fatalf("Exec(%q): %v", command, err)
	}
	for range chunks {
	}
	result := <-resultCh
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Exec(%q): exit=%d err=%v stdout=%s", command, result.ExitCode, result.Err, result.Stdout)
	}
	return string(result.Stdout)
}
