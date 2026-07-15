package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

type inputFileSourceReadError struct {
	Name string
	Path string
	Err  error
}

func (e *inputFileSourceReadError) Error() string {
	return fmt.Sprintf("read input file %s (%q): %v", e.Name, e.Path, e.Err)
}

func (e *inputFileSourceReadError) Unwrap() error { return e.Err }

type inputFileBlobStoreError struct {
	Name string
	Path string
	Err  error
}

func (e *inputFileBlobStoreError) Error() string {
	return fmt.Sprintf("put input file %s from %q: %v", e.Name, e.Path, e.Err)
}

func (e *inputFileBlobStoreError) Unwrap() error { return e.Err }

// validateSuppliedInputFiles checks the --input-files supply against the
// workflow's declared top-level input_files contract, BEFORE any state is
// created on disk. The contract mirrors ir.validateCallInputFiles (the
// call-step file-binding contract): every DECLARED name must be supplied and
// every SUPPLIED name must be declared. A top-level `awf run` is the run-start
// equivalent of a parent call binding the child's public file inputs, so the
// required-both-directions semantics are the same. Each supplied path must also
// exist and be a readable regular file.
//
// supplied is name → host path (already parsed from the CSV); declared is the
// workflow's input_files contract (name → ArtifactContract). A nil/empty
// supplied against a non-empty declared is the "missing declared" error.
func validateSuppliedInputFiles(supplied map[string]string, declared ir.WorkflowInputFiles) error {
	// Every supplied name must be declared.
	suppliedNames := make([]string, 0, len(supplied))
	for name := range supplied {
		suppliedNames = append(suppliedNames, name)
	}
	sort.Strings(suppliedNames)
	for _, name := range suppliedNames {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf(
				"--input-files supplies unknown input file %q; the workflow declares: %s",
				name, declaredInputFileNames(declared))
		}
	}

	// Every declared name must be supplied.
	declaredNames := make([]string, 0, len(declared))
	for name := range declared {
		declaredNames = append(declaredNames, name)
	}
	sort.Strings(declaredNames)
	for _, name := range declaredNames {
		if _, ok := supplied[name]; !ok {
			return fmt.Errorf(
				"workflow declares input file %q but it was not supplied; pass --input-files %s=<path>",
				name, name)
		}
	}

	// Each supplied path must exist and be a readable regular file.
	for _, name := range suppliedNames {
		path := supplied[name]
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("--input-files %s: %w", name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("--input-files %s: %q is a directory, want a file", name, path)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("--input-files %s: %q is not readable: %w", name, path, err)
		}
		_ = f.Close()
	}
	return nil
}

// declaredInputFileNames renders the declared input-file names as a sorted,
// comma-joined list for error messages. "(none)" when the workflow declares no
// input files (so an --input-files on such a workflow reads clearly).
func declaredInputFileNames(declared ir.WorkflowInputFiles) string {
	if len(declared) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// storeInputFiles content-addresses each supplied input file into blobs and
// returns name → CAS ref. Empty supply yields a nil map (omitempty in
// run.started). Paths were existence/readability-checked by
// validateSuppliedInputFiles; a read error here is an unexpected mid-run race.
func storeInputFiles(blobs state.Blobs, supplied map[string]string) (map[string]string, error) {
	return storeInputFilesWithRead(blobs, supplied, os.ReadFile)
}

func storeInputFilesWithRead(blobs state.Blobs, supplied map[string]string, readFile func(string) ([]byte, error)) (map[string]string, error) {
	if len(supplied) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(supplied))
	names := make([]string, 0, len(supplied))
	for name := range supplied {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := supplied[name]
		b, err := readFile(path)
		if err != nil {
			return nil, &inputFileSourceReadError{Name: name, Path: path, Err: err}
		}
		ref, err := blobs.Put(b)
		if err != nil {
			return nil, &inputFileBlobStoreError{Name: name, Path: path, Err: err}
		}
		out[name] = ref
	}
	return out, nil
}

func reportInputFilesFailure(stderr io.Writer, stateRoot, blobsDir string, err error, lookup stateIdentityLookup) int {
	var sourceErr *inputFileSourceReadError
	if errors.As(err, &sourceErr) {
		fprintf(stderr, "awf run: store run input files: %v\n", err)
		return ExitUsage
	}
	return reportStateFailure(stderr, "awf run", "store run input files", stateRoot, blobsDir, err, lookup, stateFailureInfra)
}
