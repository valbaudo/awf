package loader

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

type safePathPolicy struct {
	kind           string
	allowDot       bool
	requiredSuffix string
}

type safePathError struct {
	Code    string
	Message string
	Err     error
}

func (e *safePathError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *safePathError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func safeRootRelPath(declared string, policy safePathPolicy) (string, error) {
	if declared == "" {
		return "", pathError("AWF_PATH_EMPTY", "empty path not permitted", nil)
	}
	if strings.ContainsRune(declared, '\\') {
		return "", pathError("AWF_PATH_BACKSLASH", fmt.Sprintf("backslash not permitted in %s paths; use forward slash", policy.kind), nil)
	}
	if strings.ContainsFunc(declared, unicode.IsControl) {
		return "", pathError("AWF_PATH_CONTROL", fmt.Sprintf("control characters are not permitted in %s paths", policy.kind), nil)
	}
	if path.IsAbs(declared) || filepath.IsAbs(declared) {
		return "", pathError("AWF_PATH_ABSOLUTE", "absolute path not permitted (must be relative to the workflow directory)", nil)
	}

	clean := path.Clean(declared)
	if clean == "." && !policy.allowDot {
		return "", pathError("AWF_PATH_DOT", "dot path not permitted", nil)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", pathError("AWF_PATH_ESCAPE", fmt.Sprintf("path escapes the workflow directory after cleaning: %q", clean), nil)
	}
	if clean != "." && !fs.ValidPath(clean) {
		return "", pathError("AWF_PATH_INVALID", fmt.Sprintf("path is not a valid slash-separated manifest path: %q", clean), nil)
	}
	if policy.requiredSuffix != "" && !strings.HasSuffix(clean, policy.requiredSuffix) {
		return "", pathError("AWF_PATH_SUFFIX", fmt.Sprintf("path must end with %s", policy.requiredSuffix), nil)
	}
	local, err := filepath.Localize(clean)
	if err != nil {
		return "", pathError("AWF_PATH_INVALID", "path cannot be localized for this platform", err)
	}
	if !filepath.IsLocal(local) {
		return "", pathError("AWF_PATH_NONLOCAL", "localized path is not local", nil)
	}
	return clean, nil
}

func pathError(code, message string, err error) error {
	if err == nil {
		err = errors.New(message)
	}
	return &safePathError{Code: code, Message: message, Err: err}
}
