package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	dockerContainer "github.com/docker/docker/api/types/container"
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

func TestCreateFailureCleanupUsesDetachedContextAndJoinsCleanupError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			writeJSON(t, w, map[string]any{"Id": "created"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/created/archive"):
			events = append(events, "prepare-fail")
			cancel()
			http.Error(w, `{"message":"prepare denied"}`, http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/created"):
			events = append(events, "remove-fail")
			if r.URL.Query().Get("force") != "1" {
				t.Errorf("force = %q, want 1", r.URL.Query().Get("force"))
			}
			http.Error(w, `{"message":"cleanup denied"}`, http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	b := backendForServer(t, srv)
	_, err := b.Create(ctx, container.ContainerSpec{Name: "lab", Image: "example.invalid/image@sha256:abc"})
	if err == nil {
		t.Fatal("Create error = nil, want joined prepare and cleanup errors")
	}
	if !strings.Contains(err.Error(), "prepare") || !strings.Contains(err.Error(), "cleanup denied") {
		t.Fatalf("Create error = %v, want both prepare and cleanup causes", err)
	}
	want := []string{"create", "prepare-fail", "remove-fail"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestRestoreFailureCleanupUsesDetachedContextAndJoinsCleanupError(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	var diff bytes.Buffer
	dw := newDiffTarWriter(&diff, 1<<20)
	if err := dw.Close(); err != nil {
		t.Fatalf("diff Close: %v", err)
	}
	blobRef, err := blobs.Put(diff.Bytes())
	if err != nil {
		t.Fatalf("blobs.Put: %v", err)
	}
	ref, err := formatSnapshotRef(blobRef, "example.invalid/image@sha256:abc", snapshotCmdSpec{Cmd: []string{"sleep", "infinity"}})
	if err != nil {
		t.Fatalf("formatSnapshotRef: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			writeJSON(t, w, map[string]any{"Id": "restored"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/restored/archive"):
			events = append(events, "snapshot-fail")
			cancel()
			http.Error(w, `{"message":"snapshot denied"}`, http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/restored"):
			events = append(events, "remove-fail")
			if r.URL.Query().Get("force") != "1" {
				t.Errorf("force = %q, want 1", r.URL.Query().Get("force"))
			}
			http.Error(w, `{"message":"cleanup denied"}`, http.StatusInternalServerError)
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
	b, err := New(cli, "test-restore-cleanup", blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Restore(ctx, ref, "lab")
	if err == nil {
		t.Fatal("Restore error = nil, want joined snapshot and cleanup errors")
	}
	if !strings.Contains(err.Error(), "CopyToContainer") || !strings.Contains(err.Error(), "cleanup denied") {
		t.Fatalf("Restore error = %v, want both copy and cleanup causes", err)
	}
	want := []string{"create", "snapshot-fail", "remove-fail"}
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
	deletePath := "/work/old ' file"
	if err := dw.WriteDeletes([]string{deletePath}); err != nil {
		t.Fatalf("WriteDeletes: %v", err)
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
	hs := restoreHandshakeForRef(ref)

	var events []string
	archiveCall := 0
	inspectCall := 0
	markers := map[string]string{
		hs.PreparedPath: "stale-prepared\n",
		hs.ReleasePath:  "stale-release\n",
		hs.HandoffPath:  "stale-handoff\n",
		hs.AckPath:      "stale-ack\n",
	}
	var createdConfig struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			events = append(events, "create")
			if err := json.NewDecoder(r.Body).Decode(&createdConfig); err != nil {
				t.Errorf("decode create config: %v", err)
			}
			writeJSON(t, w, map[string]any{"Id": "restored"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/restored/archive"):
			archiveCall++
			if r.URL.Query().Get("copyUIDGID") != "true" {
				t.Errorf("archive call %d copyUIDGID = %q, want true", archiveCall, r.URL.Query().Get("copyUIDGID"))
			}
			switch archiveCall {
			case 1:
				events = append(events, "snapshot")
			case 2:
				events = append(events, "prepare")
			case 3:
				events = append(events, "seed")
				got := readTarFiles(t, r.Body)
				for _, path := range []string{hs.PreparedPath, hs.ReleasePath, hs.HandoffPath, hs.AckPath} {
					name := strings.TrimPrefix(path, "/")
					if got[name] != hs.ResetToken {
						t.Errorf("seed marker %q = %q, want %q", name, got[name], hs.ResetToken)
					}
					markers[path] = got[name]
				}
				w.WriteHeader(http.StatusOK)
				return
			case 4:
				events = append(events, "release")
				got := readTarFiles(t, r.Body)
				markers[hs.ReleasePath] = got[strings.TrimPrefix(hs.ReleasePath, "/")]
				if markers[hs.ReleasePath] == hs.ReleaseToken {
					markers[hs.HandoffPath] = hs.HandoffToken
				}
				w.WriteHeader(http.StatusOK)
				return
			case 5:
				events = append(events, "ack")
				got := readTarFiles(t, r.Body)
				markers[hs.AckPath] = got[strings.TrimPrefix(hs.AckPath, "/")]
				if markers[hs.AckPath] == hs.AckToken {
					delete(markers, hs.PreparedPath)
					delete(markers, hs.ReleasePath)
					delete(markers, hs.HandoffPath)
					delete(markers, hs.AckPath)
					events = append(events, "exec")
				}
				w.WriteHeader(http.StatusOK)
				return
			default:
				t.Errorf("unexpected archive call %d", archiveCall)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/restored/start"):
			events = append(events, "start")
			if markers[hs.PreparedPath] != hs.ResetToken || markers[hs.ReleasePath] != hs.ResetToken {
				t.Errorf("start observed unreset markers: %#v", markers)
			}
			markers[hs.PreparedPath] = hs.PreparedToken
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/restored/archive"):
			path := r.URL.Query().Get("path")
			content, ok := markers[path]
			if !ok {
				events = append(events, "handoff-absent")
				http.Error(w, `{"message":"path not found"}`, http.StatusNotFound)
				return
			}
			switch path {
			case hs.PreparedPath:
				events = append(events, "prepared")
			case hs.HandoffPath:
				events = append(events, "handoff")
			default:
				t.Errorf("unexpected marker read %q", path)
			}
			writeContainerPathTar(t, w, path, content)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/restored/json"):
			inspectCall++
			if inspectCall == 1 {
				events = append(events, "handoff-inspect")
			} else {
				events = append(events, "readiness-inspect")
			}
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
	wantEntrypoint := []string{"sh", "-c", restoreDeleteWrapperScript, "awf-restore"}
	if strings.Join(createdConfig.Entrypoint, "\x00") != strings.Join(wantEntrypoint, "\x00") {
		t.Errorf("created Entrypoint = %#v, want %#v", createdConfig.Entrypoint, wantEntrypoint)
	}
	wantCmd := restoreWrapperArgs([]string{deletePath}, hs, "/work/.awf", "/tmp/awf", []string{"sleep", "infinity"})
	if strings.Join(createdConfig.Cmd, "\x00") != strings.Join(wantCmd, "\x00") {
		t.Errorf("created Cmd = %#v, want %#v", createdConfig.Cmd, wantCmd)
	}
	want := []string{"create", "snapshot", "prepare", "seed", "start", "prepared", "release", "handoff", "ack", "exec", "handoff-absent", "handoff-inspect", "readiness-inspect"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func writeContainerPathTar(t *testing.T, w http.ResponseWriter, path, content string) {
	t.Helper()
	statJSON, err := json.Marshal(dockerContainer.PathStat{Name: path, Size: int64(len(content)), Mode: 0o600})
	if err != nil {
		t.Fatalf("marshal path stat: %v", err)
	}
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path, "/"), Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(content))}); err != nil {
		t.Errorf("write path tar header: %v", err)
		return
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Errorf("write path tar body: %v", err)
		return
	}
	if err := tw.Close(); err != nil {
		t.Errorf("close path tar: %v", err)
	}
}

func readTarFiles(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	got := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body %q: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(body)
	}
}

func TestRestoreDeleteWrapperExitReportsShellRequirementAndCleansUp(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	var diff bytes.Buffer
	dw := newDiffTarWriter(&diff, 1<<20)
	if err := dw.WriteDeletes([]string{"/work/removed"}); err != nil {
		t.Fatalf("WriteDeletes: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("diff Close: %v", err)
	}
	blobRef, err := blobs.Put(diff.Bytes())
	if err != nil {
		t.Fatalf("blobs.Put: %v", err)
	}
	ref, err := formatSnapshotRef(blobRef, "example.invalid/no-shell@sha256:abc", snapshotCmdSpec{Cmd: []string{"original"}})
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
			switch archiveCall {
			case 1:
				events = append(events, "snapshot")
			case 2:
				events = append(events, "prepare")
			case 3:
				events = append(events, "seed")
			default:
				t.Errorf("unexpected archive call %d", archiveCall)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/restored/start"):
			events = append(events, "start")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/restored/archive"):
			events = append(events, "prepared-missing")
			http.Error(w, `{"message":"path not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/restored/json"):
			events = append(events, "inspect-exited")
			writeJSON(t, w, map[string]any{"Id": "restored", "State": map[string]any{"Running": false}})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/restored"):
			events = append(events, "remove")
			w.WriteHeader(http.StatusNoContent)
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
	b, err := New(cli, "test-no-shell", blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Restore(context.Background(), ref, "lab")
	if err == nil || !strings.Contains(err.Error(), "must provide POSIX sh") {
		t.Fatalf("Restore error = %v, want explicit POSIX sh requirement", err)
	}
	want := []string{"create", "snapshot", "prepare", "seed", "start", "prepared-missing", "inspect-exited", "remove"}
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
