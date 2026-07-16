package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
)

func fakeStateIdentity(uid, gid int, provenance string) stateIdentityLookup {
	return func() (stateIdentity, error) {
		return stateIdentity{UID: uid, GID: gid, ElevationProvenance: provenance}, nil
	}
}

func TestStateDirWriteCreateCanonicalizesAndUsesPrivateMode(t *testing.T) {
	parent := t.TempDir()
	root, err := accessStateDir(filepath.Join(parent, "nested", "..", "state"), stateWriteCreate, defaultStateIdentity)
	if err != nil {
		t.Fatalf("accessStateDir: %v", err)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("root = %q, want absolute", root)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("state mode = %#o, want no group/world permissions", got)
	}
}

func TestStateDirWriteRejectsForeignOwnerBeforeCreatingChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership test")
	}
	root := t.TempDir()
	actual := os.Geteuid()
	foreignCurrent := actual + 1
	_, err := accessStateDir(root, stateWriteExisting, fakeStateIdentity(foreignCurrent, os.Getegid(), ""))
	if !errors.Is(err, errUnsafeStateRoot) {
		t.Fatalf("accessStateDir error = %v, want errUnsafeStateRoot", err)
	}
	for _, child := range []string{"blobs", "work", "live", "runs", "control"} {
		if _, statErr := os.Stat(filepath.Join(root, child)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s was created before ownership refusal: %v", child, statErr)
		}
	}
}

func TestStateDirReadOnlyAcceptsForeignOwnerAndDoesNotCreate(t *testing.T) {
	root := t.TempDir()
	got, err := accessStateDir(root, stateReadOnly, fakeStateIdentity(os.Geteuid()+1, os.Getegid(), ""))
	if err != nil {
		t.Fatalf("read-only foreign-owned root rejected: %v", err)
	}
	if got == "" {
		t.Fatal("read-only root is empty")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only access created entries: %v", entries)
	}
}

func TestStateDirWriteRejectsGroupOrWorldWritableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode test")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	_, err := accessStateDir(root, stateWriteExisting, defaultStateIdentity)
	if !errors.Is(err, errUnsafeStateRoot) {
		t.Fatalf("accessStateDir error = %v, want errUnsafeStateRoot", err)
	}
}

func TestStateDirUnsupportedPlatformDoesNotApplyUnixPermissionBits(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := canonicalStatePath(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := func(path string) (statePathMetadata, error) {
		if path != canonicalRoot {
			t.Fatalf("metadata path = %q, want %q", path, canonicalRoot)
		}
		return statePathMetadata{
			Mode:            os.ModeDir | 0o777,
			OwnerUID:        0,
			OwnerGID:        0,
			OwnershipKnown:  false,
			UnixPermissions: false,
		}, nil
	}

	got, err := accessStateDirWithMetadata(root, stateWriteExisting, fakeStateIdentity(1000, 1000, ""), metadata)
	if err != nil {
		t.Fatalf("unsupported-platform write rejected Unix mode bits: %v", err)
	}
	if got != canonicalRoot {
		t.Fatalf("root = %q, want %q", got, canonicalRoot)
	}
}

func TestStateDirUnixMetadataCarriesUIDGIDAndMode(t *testing.T) {
	got := unixStatePathMetadata(os.ModeDir|0o700, 42, 84)
	if !got.UnixPermissions || !got.OwnershipKnown {
		t.Fatalf("Unix metadata flags = %+v, want Unix permissions and known ownership", got)
	}
	if got.OwnerUID != 42 || got.OwnerGID != 84 || got.Mode != os.ModeDir|0o700 {
		t.Fatalf("Unix metadata = %+v, want UID 42 GID 84 mode %s", got, os.ModeDir|0o700)
	}
}

func TestStateErrorUnsupportedPlatformReportsPathAndModeWithoutOwnerClaim(t *testing.T) {
	root := t.TempDir()
	metadata := func(string) (statePathMetadata, error) {
		return statePathMetadata{Mode: os.ModeDir | 0o777}, nil
	}
	err := &os.PathError{Op: "open", Path: root, Err: syscall.EACCES}
	got := formatStateErrorWithMetadata("access state directory", root, root, err, fakeStateIdentity(-1, -1, ""), metadata)
	for _, want := range []string{root, "mode -rwxrwxrwx"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "owner UID") || strings.Contains(got, "owner GID") {
		t.Fatalf("unsupported-platform diagnostic claimed an owner:\n%s", got)
	}
}

func TestStateDirMutationRejectsElevationProvenanceBeforeCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-state")
	_, err := accessStateDir(root, stateWriteCreate, fakeStateIdentity(0, 0, "sudo (SUDO_UID)"))
	if !errors.Is(err, errElevatedStateMutation) {
		t.Fatalf("accessStateDir error = %v, want errElevatedStateMutation", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("state root created despite elevation refusal: %v", statErr)
	}
}

func TestStateIdentityDetectsSupportedElevationProvenance(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "sudo uid", env: map[string]string{"SUDO_UID": "1000"}, want: "sudo"},
		{name: "sudo user", env: map[string]string{"SUDO_USER": "alice"}, want: "sudo"},
		{name: "doas", env: map[string]string{"DOAS_USER": "alice"}, want: "doas"},
		{name: "pkexec", env: map[string]string{"PKEXEC_UID": "1000"}, want: "pkexec"},
		{name: "genuine root", env: map[string]string{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stateIdentityWithEnvironment(stateIdentity{UID: 0, GID: 0}, func(key string) string { return tt.env[key] })
			if !strings.Contains(got.ElevationProvenance, tt.want) {
				t.Fatalf("provenance = %q, want substring %q", got.ElevationProvenance, tt.want)
			}
		})
	}
}

func TestStateFailureExitClassification(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runs", "r1", "log")
	err := &os.PathError{Op: "open", Path: path, Err: syscall.EPERM}
	for _, tt := range []struct {
		name   string
		policy stateFailurePolicy
		want   int
	}{
		{name: "shared commands", policy: stateFailureInfra, want: ExitInfra},
		{name: "outputs contract exception", policy: stateFailureOutputs, want: ExitUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := reportStateFailure(&stderr, "awf test", "open run log", root, path, err, defaultStateIdentity, tt.policy); got != tt.want {
				t.Fatalf("exit = %d, want %d", got, tt.want)
			}
			if !strings.Contains(stderr.String(), statePermissionHint) {
				t.Fatalf("missing permission hint: %s", stderr.String())
			}
		})
	}
}

func TestStateErrorDiagnosticPreservesDeepPermissionContext(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "blobs", "sha256", "ab", "blob")
	err := &os.PathError{Op: "open", Path: deep, Err: syscall.EACCES}
	got := formatStateError("read blob shard", root, deep, err, defaultStateIdentity)
	for _, want := range []string{
		"read blob shard",
		deep,
		"permission denied",
		"current UID",
		"owner UID",
		"AWF does not elevate privileges",
		"Do not rerun awf with sudo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic missing %q:\n%s", want, got)
		}
	}
}

func TestRunRejectsInjectedElevationBeforeStateCreation(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	runner := &Runner{
		Backend:        container.NewFake(),
		IDGen:          &clock.Fake{IDs: []string{"elevated-run"}},
		identityLookup: fakeStateIdentity(0, 0, "pkexec (PKEXEC_UID)"),
	}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != ExitInfra {
		t.Fatalf("rc = %d, want ExitInfra; stderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state root created despite elevation refusal: %v", err)
	}
	if !strings.Contains(stderr.String(), "pkexec") || !strings.Contains(stderr.String(), "Do not rerun awf with sudo") {
		t.Fatalf("missing elevation diagnostic: %s", stderr.String())
	}
}
