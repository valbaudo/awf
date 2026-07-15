package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/valbaudo/awf/state"
)

type failingInputBlobs struct{ err error }

func (b failingInputBlobs) Put([]byte) (string, error) { return "", b.err }
func (b failingInputBlobs) Get(string) ([]byte, error) { return nil, b.err }

func TestParseInputFilesCSV(t *testing.T) {
	eq := func(t *testing.T, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("got[%q] = %q, want %q (full: %v)", k, got[k], v, got)
			}
		}
	}
	t.Run("repeated form binds each occurrence", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x", "b=y"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x", "b": "y"})
	})
	t.Run("legacy single comma-separated value", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x,b=y"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x", "b": "y"})
	})
	t.Run("comma in path via repeated form is preserved", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"doc=/tmp/a,b.txt", "img=/tmp/c.png"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"doc": "/tmp/a,b.txt", "img": "/tmp/c.png"})
	})
	t.Run("empty yields nil map", func(t *testing.T) {
		if got, err := parseInputFilesCSV(nil); err != nil || got != nil {
			t.Fatalf("nil: got (%v, %v), want (nil, nil)", got, err)
		}
		if got, err := parseInputFilesCSV([]string{""}); err != nil || got != nil {
			t.Fatalf("[\"\"]: got (%v, %v), want (nil, nil)", got, err)
		}
		// A lone separators-only value splits to zero pairs → nil map, not empty.
		if got, err := parseInputFilesCSV([]string{","}); err != nil || got != nil {
			t.Fatalf("[\",\"]: got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("malformed entry errors", func(t *testing.T) {
		if _, err := parseInputFilesCSV([]string{"noeq"}); err == nil {
			t.Fatal("want error for entry without '='")
		}
	})
	t.Run("duplicate name errors", func(t *testing.T) {
		if _, err := parseInputFilesCSV([]string{"a=x", "a=y"}); err == nil {
			t.Fatal("want error for duplicate name")
		}
	})
	t.Run("single comma-free entry", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x"})
	})
}

func TestStoreInputFilesSourceReadRaceStaysUserFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "document.txt")
	readFailure := &os.PathError{Op: "read", Path: source, Err: syscall.EACCES}
	_, err := storeInputFilesWithRead(state.NewInMemoryBlobs(), map[string]string{"document": source}, func(path string) ([]byte, error) {
		if path != source {
			t.Fatalf("read path = %q, want %q", path, source)
		}
		return nil, readFailure
	})
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("storeInputFiles error = %v, want EACCES in %%w chain", err)
	}
	var sourceErr *inputFileSourceReadError
	if !errors.As(err, &sourceErr) || sourceErr.Path != source {
		t.Fatalf("storeInputFiles error = %T %v, want typed source error for %q", err, err, source)
	}

	var stderr bytes.Buffer
	rc := reportInputFilesFailure(&stderr, t.TempDir(), filepath.Join(t.TempDir(), "blobs"), err, fakeStateIdentity(1000, 1000, ""))
	if rc != ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), source) {
		t.Fatalf("stderr missing source path %q: %s", source, stderr.String())
	}
	if strings.Contains(stderr.String(), statePermissionHint) || strings.Contains(stderr.String(), "state directory") {
		t.Fatalf("user source read received state/no-sudo guidance: %s", stderr.String())
	}
}

func TestStoreInputFilesBlobPutFailureUsesDeepStateDiagnostic(t *testing.T) {
	root := t.TempDir()
	blobsDir := filepath.Join(root, "blobs")
	deep := filepath.Join(blobsDir, "sha256", "ab", "blob.tmp")
	putFailure := &os.PathError{Op: "create", Path: deep, Err: syscall.EPERM}
	_, err := storeInputFilesWithRead(failingInputBlobs{err: putFailure}, map[string]string{"document": "/input/document.txt"}, func(string) ([]byte, error) {
		return []byte("document"), nil
	})
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("storeInputFiles error = %v, want EPERM in %%w chain", err)
	}
	var storeErr *inputFileBlobStoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("storeInputFiles error = %T %v, want typed blob-store error", err, err)
	}

	var stderr bytes.Buffer
	rc := reportInputFilesFailure(&stderr, root, blobsDir, err, fakeStateIdentity(1000, 1000, ""))
	if rc != ExitInfra {
		t.Fatalf("rc = %d, want ExitInfra; stderr=%s", rc, stderr.String())
	}
	for _, want := range []string{deep, "operation not permitted", statePermissionHint} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %s", want, stderr.String())
		}
	}
}
