package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

func TestExtractToContainerSetsRequestedOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownership extractOwnership
		want      string
	}{
		{name: "preserve archive ownership", ownership: preserveArchiveOwnership, want: ""},
		{name: "own by container user", ownership: ownByContainerUser, want: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotOwnership string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Query().Get("path")
				gotOwnership = r.URL.Query().Get("copyUIDGID")
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cli, err := client.NewClientWithOpts(
				client.WithHost(srv.URL),
				client.WithVersion("1.44"),
				client.WithHTTPClient(srv.Client()),
			)
			if err != nil {
				t.Fatalf("NewClientWithOpts: %v", err)
			}
			b, err := New(cli, "test-extract", state.NewInMemoryBlobs())
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if err := b.extractToContainer(context.Background(), "container-id", "/", emptyTar(t), tc.ownership); err != nil {
				t.Fatalf("extractToContainer: %v", err)
			}
			if gotPath != "/" {
				t.Errorf("path = %q, want /", gotPath)
			}
			if gotOwnership != tc.want {
				t.Errorf("copyUIDGID = %q, want %q", gotOwnership, tc.want)
			}
		})
	}
}

func TestPrepareRuntimeDirsCopiesExactOwnedDirectoryArchive(t *testing.T) {
	var gotPath, gotOwnership string
	var headers []*tar.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		gotOwnership = r.URL.Query().Get("copyUIDGID")
		tr := tar.NewReader(r.Body)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("tar.Next: %v", err)
				break
			}
			copy := *hdr
			headers = append(headers, &copy)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(srv.URL),
		client.WithVersion("1.44"),
		client.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	b, err := New(cli, "test-runtime-dirs", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.prepareRuntimeDirs(context.Background(), "container-id"); err != nil {
		t.Fatalf("prepareRuntimeDirs: %v", err)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotOwnership != "true" {
		t.Errorf("copyUIDGID = %q, want true", gotOwnership)
	}
	if len(headers) != 2 {
		t.Fatalf("headers = %d, want 2", len(headers))
	}
	wantNames := []string{"work/.awf/", "tmp/awf/"}
	for i, hdr := range headers {
		if hdr.Name != wantNames[i] {
			t.Errorf("headers[%d].Name = %q, want %q", i, hdr.Name, wantNames[i])
		}
		if hdr.Typeflag != tar.TypeDir {
			t.Errorf("headers[%d].Typeflag = %d, want TypeDir", i, hdr.Typeflag)
		}
		if hdr.Mode != 0o755 {
			t.Errorf("headers[%d].Mode = %#o, want 0755", i, hdr.Mode)
		}
	}
}

func TestWorkspaceExtractionNormalizesOwnership(t *testing.T) {
	var ownership []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownership = append(ownership, r.URL.Query().Get("copyUIDGID"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(srv.URL),
		client.WithVersion("1.44"),
		client.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	b, err := New(cli, "test-workspace-extract", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := container.Handle{Name: "lab", ID: "container-id"}
	b.handles[h.ID] = registeredContainer{kind: kindImage, dockerID: h.ID}

	tree, err := container.BuildTreeTar(map[string][]byte{"nested/file.txt": []byte("tree")})
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{
			name: "CopyTo",
			run: func() error {
				return b.CopyTo(context.Background(), h, []container.InputFile{{Path: "/work/input.txt", Content: []byte("input")}})
			},
		},
		{
			name: "WriteFileAt",
			run: func() error {
				return b.WriteFileAt(context.Background(), h, "/work/file.txt", []byte("file"))
			},
		},
		{
			name: "WriteTreeAt",
			run: func() error {
				return b.WriteTreeAt(context.Background(), h, "/work/tree", tree)
			},
		},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			if err := op.run(); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
		})
	}
	if len(ownership) != len(operations) {
		t.Fatalf("archive calls = %d, want %d", len(ownership), len(operations))
	}
	for i, got := range ownership {
		if got != "true" {
			t.Errorf("archive call %d copyUIDGID = %q, want true", i, got)
		}
	}
}

func TestCreatePreparesRuntimeDirsBeforeStart(t *testing.T) {
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			writeJSON(t, w, map[string]any{"Id": "created"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/created/archive"):
			events = append(events, "prepare")
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/created/start"):
			events = append(events, "start")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/created/json"):
			events = append(events, "inspect")
			writeJSON(t, w, map[string]any{"Id": "created", "State": map[string]any{"Running": true}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	b := backendForServer(t, srv)
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab", Image: "example.invalid/image@sha256:abc"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "created" {
		t.Errorf("Handle.ID = %q, want created", h.ID)
	}
	want := []string{"create", "prepare", "start", "inspect"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestCreateRuntimeDirFailureForceRemovesPartialContainer(t *testing.T) {
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			writeJSON(t, w, map[string]any{"Id": "created"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/created/archive"):
			events = append(events, "prepare-fail")
			http.Error(w, `{"message":"read-only filesystem"}`, http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/created"):
			events = append(events, "remove")
			if r.URL.Query().Get("force") != "1" {
				t.Errorf("force = %q, want 1", r.URL.Query().Get("force"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	b := backendForServer(t, srv)
	_, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab", Image: "example.invalid/image@sha256:abc"})
	if err == nil || !strings.Contains(err.Error(), "prepareRuntimeDirs") {
		t.Fatalf("Create error = %v, want prepareRuntimeDirs error", err)
	}
	want := []string{"create", "prepare-fail", "remove"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestRestoreExtractsSnapshotAndRuntimeDirsBeforeStart(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	var diff bytes.Buffer
	dw := newDiffTarWriter(&diff, 1<<20)
	if err := dw.WriteRegular("/work/restored.txt", bytes.NewReader([]byte("restored")), int64(len("restored"))); err != nil {
		t.Fatalf("WriteRegular: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("diff Close: %v", err)
	}
	blobRef, err := blobs.Put(diff.Bytes())
	if err != nil {
		t.Fatalf("blobs.Put: %v", err)
	}
	ref, err := formatSnapshotRef(blobRef, "example.invalid/nonroot@sha256:abc", snapshotCmdSpec{Cmd: []string{"sleep", "infinity"}})
	if err != nil {
		t.Fatalf("formatSnapshotRef: %v", err)
	}

	var events []string
	archiveCall := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			writeJSON(t, w, map[string]any{"Id": "restored"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/restored/archive"):
			archiveCall++
			if r.URL.Query().Get("copyUIDGID") != "true" {
				t.Errorf("archive call %d copyUIDGID = %q, want true", archiveCall, r.URL.Query().Get("copyUIDGID"))
			}
			if archiveCall == 1 {
				events = append(events, "snapshot")
			} else {
				events = append(events, "prepare")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/restored/start"):
			events = append(events, "start")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/restored/json"):
			events = append(events, "inspect")
			writeJSON(t, w, map[string]any{"Id": "restored", "State": map[string]any{"Running": true}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	cli, err := dockerClientForServer(srv)
	if err != nil {
		t.Fatalf("dockerClientForServer: %v", err)
	}
	b, err := New(cli, "test-restore-order", blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Restore(context.Background(), ref, "lab"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := []string{"create", "snapshot", "prepare", "start", "inspect"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestPrepareComposeRuntimeDirsPreparesEveryServiceIndependently(t *testing.T) {
	var listedServices []string
	var preparedIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			filters := r.URL.Query().Get("filters")
			service := ""
			for _, candidate := range []string{"web", "worker"} {
				if strings.Contains(filters, api.ServiceLabel+"="+candidate) {
					service = candidate
					break
				}
			}
			if service == "" {
				t.Errorf("ContainerList filters missing service: %s", filters)
				http.Error(w, "missing service", http.StatusBadRequest)
				return
			}
			listedServices = append(listedServices, service)
			writeJSON(t, w, []map[string]any{{"Id": service + "-id"}})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/archive"):
			parts := strings.Split(r.URL.Path, "/")
			preparedIDs = append(preparedIDs, parts[len(parts)-2])
			if r.URL.Query().Get("copyUIDGID") != "true" {
				t.Errorf("copyUIDGID = %q, want true", r.URL.Query().Get("copyUIDGID"))
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	b := backendForServer(t, srv)
	if err := b.prepareComposeRuntimeDirs(context.Background(), "project", []string{"worker", "web"}); err != nil {
		t.Fatalf("prepareComposeRuntimeDirs: %v", err)
	}
	if got, want := strings.Join(listedServices, ","), "web,worker"; got != want {
		t.Errorf("listed services = %q, want %q", got, want)
	}
	if got, want := strings.Join(preparedIDs, ","), "web-id,worker-id"; got != want {
		t.Errorf("prepared IDs = %q, want %q", got, want)
	}
}

func backendForServer(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()
	cli, err := dockerClientForServer(srv)
	if err != nil {
		t.Fatalf("dockerClientForServer: %v", err)
	}
	b, err := New(cli, "test-server", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func dockerClientForServer(srv *httptest.Server) (*client.Client, error) {
	return client.NewClientWithOpts(
		client.WithHost(srv.URL),
		client.WithVersion("1.44"),
		client.WithHTTPClient(srv.Client()),
	)
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func emptyTar(t *testing.T) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.Close()
		_ = pw.CloseWithError(err)
	}()
	return pr
}
