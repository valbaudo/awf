package loader

import (
	"errors"
	"testing"
)

func TestSafeRootRelPathNormalizesDotSlash(t *testing.T) {
	got, err := safeRootRelPath("./sub/../compose.yml", safePathPolicy{kind: "compose"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "compose.yml" {
		t.Fatalf("safeRootRelPath = %q, want compose.yml", got)
	}
}

func TestSafeRootRelPathRejectsPathEscape(t *testing.T) {
	_, err := safeRootRelPath("../escape.yml", safePathPolicy{kind: "compose"})
	assertSafePathCode(t, err, "AWF_PATH_ESCAPE")
}

func TestSafeRootRelPathRejectsBackslashAndControls(t *testing.T) {
	for name, declared := range map[string]string{
		"backslash": `dir\compose.yml`,
		"nul":       "dir\x00compose.yml",
		"tab":       "dir\tcompose.yml",
		"carriage":  "dir\rcompose.yml",
		"linefeed":  "dir\ncompose.yml",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := safeRootRelPath(declared, safePathPolicy{kind: "compose"})
			if err == nil {
				t.Fatalf("expected error for %q", declared)
			}
		})
	}
}

func TestSafeRootRelPathAllowsDotOnlyWhenPolicyAllowsDot(t *testing.T) {
	_, err := safeRootRelPath(".", safePathPolicy{kind: "asset"})
	assertSafePathCode(t, err, "AWF_PATH_DOT")

	got, err := safeRootRelPath(".", safePathPolicy{kind: "asset", allowDot: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "." {
		t.Fatalf("safeRootRelPath = %q, want .", got)
	}
}

func TestImportRelPathRequiresAWFSuffix(t *testing.T) {
	got, err := importRelPath("./child.awf.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "child.awf.yaml" {
		t.Fatalf("importRelPath = %q, want child.awf.yaml", got)
	}

	_, err = importRelPath("child.yaml")
	assertSafePathCode(t, err, "AWF_IMPORT_PATH_INVALID")
}

func assertSafePathCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected safe path error code %s, got nil", want)
	}
	var pathErr *safePathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("err = %T %v, want *safePathError", err, err)
	}
	if pathErr.Code != want {
		t.Fatalf("safe path code = %s, want %s (err: %v)", pathErr.Code, want, err)
	}
}
