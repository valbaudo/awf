package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type stateAccessMode uint8

const (
	stateReadOnly stateAccessMode = iota
	stateWriteCreate
	stateWriteExisting
)

type stateFailurePolicy uint8

const (
	stateFailureInfra stateFailurePolicy = iota
	stateFailureOutputs
)

type stateIdentity struct {
	UID                 int
	GID                 int
	ElevationProvenance string
}

type stateIdentityLookup func() (stateIdentity, error)

// statePathMetadata separates portable file information from Unix-only
// ownership and permission semantics. Tests inject this value so policy does
// not depend on the host running the suite.
type statePathMetadata struct {
	Mode            fs.FileMode
	OwnerUID        int
	OwnerGID        int
	OwnershipKnown  bool
	UnixPermissions bool
}

type stateMetadataLookup func(string) (statePathMetadata, error)

func unixStatePathMetadata(mode fs.FileMode, uid, gid int) statePathMetadata {
	return statePathMetadata{
		Mode: mode, OwnerUID: uid, OwnerGID: gid,
		OwnershipKnown: true, UnixPermissions: true,
	}
}

var (
	errUnsafeStateRoot       = errors.New("unsafe state root")
	errElevatedStateMutation = errors.New("elevated state mutation refused")
)

const statePermissionHint = "AWF does not elevate privileges. The state directory must be accessible to the invoking user; it may have been created by an earlier sudo/root invocation. Use a user-owned --state-dir or correct the ownership outside AWF. Do not rerun awf with sudo."

type unsafeStateRootError struct {
	Path       string
	CurrentUID int
	CurrentGID int
	OwnerUID   int
	OwnerGID   int
	OwnerKnown bool
	Mode       fs.FileMode
	Reason     string
}

func (e *unsafeStateRootError) Error() string {
	details := []string{fmt.Sprintf("mode %s", e.Mode.Perm())}
	if e.OwnerKnown {
		details = append(details, fmt.Sprintf("owner UID %d, owner GID %d", e.OwnerUID, e.OwnerGID))
	}
	if e.CurrentUID >= 0 {
		details = append(details, fmt.Sprintf("current UID %d, current GID %d", e.CurrentUID, e.CurrentGID))
	}
	return fmt.Sprintf("%v: state directory %q is not safe for writes (%s): %s", errUnsafeStateRoot, e.Path, strings.Join(details, ", "), e.Reason)
}

func (e *unsafeStateRootError) Unwrap() error { return errUnsafeStateRoot }

type elevatedStateMutationError struct {
	Provenance string
}

func (e *elevatedStateMutationError) Error() string {
	return fmt.Sprintf("%v: detected %s provenance", errElevatedStateMutation, e.Provenance)
}

func (e *elevatedStateMutationError) Unwrap() error { return errElevatedStateMutation }

func defaultStateIdentity() (stateIdentity, error) {
	return stateIdentityWithEnvironment(currentStateIdentity(), os.Getenv), nil
}

func stateIdentityWithEnvironment(id stateIdentity, getenv func(string) string) stateIdentity {
	switch {
	case getenv("SUDO_UID") != "" || getenv("SUDO_USER") != "":
		id.ElevationProvenance = "sudo (SUDO_UID/SUDO_USER)"
	case getenv("DOAS_USER") != "":
		id.ElevationProvenance = "doas (DOAS_USER)"
	case getenv("PKEXEC_UID") != "":
		id.ElevationProvenance = "pkexec (PKEXEC_UID)"
	}
	return id
}

// accessStateDir is the deliberately small CLI state-access seam. It resolves
// one canonical root, applies the read-vs-write policy once, and leaves every
// descendant operation to the package that owns it. It is not a state service.
func accessStateDir(path string, mode stateAccessMode, lookup stateIdentityLookup) (string, error) {
	return accessStateDirWithMetadata(path, mode, lookup, stateDirInfo)
}

func accessStateDirWithMetadata(path string, mode stateAccessMode, lookup stateIdentityLookup, metadata stateMetadataLookup) (string, error) {
	if lookup == nil {
		lookup = defaultStateIdentity
	}
	id, err := lookup()
	if err != nil {
		return "", fmt.Errorf("look up invoking identity: %w", err)
	}
	if mode != stateReadOnly && id.ElevationProvenance != "" {
		return "", &elevatedStateMutationError{Provenance: id.ElevationProvenance}
	}

	root, err := canonicalStatePath(path)
	if err != nil {
		return "", err
	}
	if mode == stateWriteCreate {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return root, err
		}
	}
	file, err := metadata(root)
	if err != nil {
		return root, err
	}
	if !file.Mode.IsDir() {
		return root, &os.PathError{Op: "open state directory", Path: root, Err: syscallENOTDIR()}
	}
	if mode == stateReadOnly {
		dir, openErr := os.Open(root)
		if openErr != nil {
			return root, openErr
		}
		_, readErr := dir.Readdirnames(1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return root, readErr
		}
		if closeErr != nil {
			return root, closeErr
		}
		return root, nil
	}
	if file.UnixPermissions && file.OwnershipKnown && file.OwnerUID != id.UID {
		return root, &unsafeStateRootError{
			Path: root, CurrentUID: id.UID, CurrentGID: id.GID,
			OwnerUID: file.OwnerUID, OwnerGID: file.OwnerGID, OwnerKnown: true,
			Mode: file.Mode, Reason: "directory is owned by another user",
		}
	}
	if file.UnixPermissions && file.Mode.Perm()&0o022 != 0 {
		return root, &unsafeStateRootError{
			Path: root, CurrentUID: id.UID, CurrentGID: id.GID,
			OwnerUID: file.OwnerUID, OwnerGID: file.OwnerGID, OwnerKnown: file.OwnershipKnown,
			Mode: file.Mode, Reason: "directory is group- or world-writable",
		}
	}
	return root, nil
}

// canonicalStatePath resolves symlinks in the existing prefix even when the
// requested write-create root does not exist yet.
func canonicalStatePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute state directory %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return abs, err
	}

	cur := abs
	var missing []string
	for {
		if _, statErr := os.Lstat(cur); statErr == nil {
			break
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return abs, statErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
	cur, err = filepath.EvalSymlinks(cur)
	if err != nil {
		return abs, err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		cur = filepath.Join(cur, missing[i])
	}
	return filepath.Clean(cur), nil
}

func formatStateError(operation, stateRoot, path string, err error, lookup stateIdentityLookup) string {
	return formatStateErrorWithMetadata(operation, stateRoot, path, err, lookup, stateDirInfo)
}

func formatStateErrorWithMetadata(operation, stateRoot, path string, err error, lookup stateIdentityLookup, metadata stateMetadataLookup) string {
	var pathError *os.PathError
	if errors.As(err, &pathError) && pathError.Path != "" {
		path = pathError.Path
	}
	diagnosticPath, pathErr := canonicalStatePath(path)
	if pathErr != nil {
		if abs, absErr := filepath.Abs(path); absErr == nil {
			diagnosticPath = filepath.Clean(abs)
		} else {
			diagnosticPath = path
		}
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s %q: %v", operation, diagnosticPath, err)

	if lookup == nil {
		lookup = defaultStateIdentity
	}
	details := make([]string, 0, 3)
	file, fileKnown := stateMetadataForDiagnostic(diagnosticPath, stateRoot, metadata)
	if fileKnown {
		details = append(details, fmt.Sprintf("mode %s", file.Mode.Perm()))
	}
	if id, identityErr := lookup(); identityErr == nil && id.UID >= 0 {
		details = append(details, fmt.Sprintf("current UID %d, current GID %d", id.UID, id.GID))
		if fileKnown && file.OwnershipKnown {
			details = append(details, fmt.Sprintf("owner UID %d, owner GID %d", file.OwnerUID, file.OwnerGID))
		} else {
			var unsafe *unsafeStateRootError
			if errors.As(err, &unsafe) && unsafe.OwnerKnown {
				details = append(details, fmt.Sprintf("owner UID %d, owner GID %d", unsafe.OwnerUID, unsafe.OwnerGID))
			}
		}
	}
	if len(details) > 0 {
		fmt.Fprintf(b, " (%s)", strings.Join(details, "; "))
	}
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, errUnsafeStateRoot) || errors.Is(err, errElevatedStateMutation) {
		b.WriteString("\n")
		b.WriteString(statePermissionHint)
	}
	return b.String()
}

func stateMetadataForDiagnostic(path, stateRoot string, metadata stateMetadataLookup) (statePathMetadata, bool) {
	root, err := canonicalStatePath(stateRoot)
	if err != nil {
		root, err = filepath.Abs(stateRoot)
		if err != nil {
			root = filepath.Clean(stateRoot)
		}
	}
	cur := filepath.Clean(path)
	for {
		if file, err := metadata(cur); err == nil {
			return file, true
		}
		if cur == root {
			break
		}
		parent := filepath.Dir(cur)
		rel, relErr := filepath.Rel(root, parent)
		if parent == cur || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			break
		}
		cur = parent
	}
	return statePathMetadata{}, false
}

func reportStateFailure(stderr io.Writer, command, operation, stateRoot, path string, err error, lookup stateIdentityLookup, policy stateFailurePolicy) int {
	fprintf(stderr, "%s: %s\n", command, formatStateError(operation, stateRoot, path, err, lookup))
	if policy == stateFailureOutputs {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, errUnsafeStateRoot) || errors.Is(err, errElevatedStateMutation) {
			return ExitUsage
		}
		return ExitRunFailed
	}
	return ExitInfra
}

// defaultStateDir is the value seeded as the default for every --state-dir flag:
// the AWF_STATE_DIR environment variable when set and non-empty, otherwise ".awf".
//
// Seeding the flag DEFAULT (rather than post-processing the flag value) gives the
// precedence explicit flag > AWF_STATE_DIR > .awf for free: pflag uses the default
// only when --state-dir is absent, so an explicit flag always wins and no per-
// call-site resolution logic is needed.
func defaultStateDir() string {
	if v := os.Getenv("AWF_STATE_DIR"); v != "" {
		return v
	}
	return ".awf"
}
