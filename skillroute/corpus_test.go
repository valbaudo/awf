package skillroute

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestNewCorpusExtractsSkillDirs(t *testing.T) {
	files := []File{
		file("sql/SKILL.md", "Find SQL injection bugs."),
		file("sql/examples/payload.txt", "' OR 1=1 --"),
		file("xss/SKILL.md", "Find cross-site scripting bugs."),
	}

	corpus, err := NewCorpus("security", files)
	if err != nil {
		t.Fatalf("NewCorpus returned error: %v", err)
	}

	if got, want := corpus.SkillIDs(), []string{"sql", "xss"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillIDs() = %v, want %v", got, want)
	}

	staged, ok := corpus.StageFiles("sql", "/work/.awf/skills")
	if !ok {
		t.Fatal("StageFiles(sql) ok = false, want true")
	}
	gotPaths := pathsOf(staged)
	wantPaths := []string{
		"/work/.awf/skills/sql/SKILL.md",
		"/work/.awf/skills/sql/examples/payload.txt",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("staged paths = %v, want %v", gotPaths, wantPaths)
	}
	if string(staged[1].Content) != "' OR 1=1 --" {
		t.Fatalf("staged nested content = %q, want preserved payload", staged[1].Content)
	}
}

func TestNewCorpusRejectsInvalidLayout(t *testing.T) {
	cases := []struct {
		name string
		file File
		kind IssueKind
	}{
		{
			name: "root-file",
			file: file("README.md", "root files are not skills"),
			kind: IssueRootFile,
		},
		{
			name: "missing-skill-md",
			file: file("noskill/notes.md", "notes only"),
			kind: IssueMissingSkillMD,
		},
		{
			name: "unsafe-id",
			file: file("bad.id/SKILL.md", "bad id"),
			kind: IssueInvalidSkillID,
		},
		{
			name: "unsafe-path",
			file: file("../sql/SKILL.md", "escape"),
			kind: IssueInvalidPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCorpus("security", []File{tc.file})
			if err == nil {
				t.Fatal("NewCorpus returned nil error, want layout error")
			}
			var issues IssuesError
			if !errors.As(err, &issues) {
				t.Fatalf("error type = %T, want IssuesError", err)
			}
			if !hasIssueKind(issues, tc.kind) {
				t.Fatalf("issues = %#v, want kind %q", issues, tc.kind)
			}
		})
	}
}

func TestValidateFilesReportsAllIssues(t *testing.T) {
	issues := ValidateFiles([]File{
		file("README.md", "root"),
		file("bad.id/SKILL.md", "invalid id"),
		file("noskill/notes.md", "missing skill md"),
	})

	if len(issues) != 3 {
		t.Fatalf("len(issues) = %d, want 3: %#v", len(issues), issues)
	}
	got := issueKinds(issues)
	want := []IssueKind{IssueRootFile, IssueInvalidSkillID, IssueMissingSkillMD}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issue kinds = %v, want %v", got, want)
	}
	gotPaths := []string{issues[0].Path, issues[1].Path, issues[2].Path}
	wantPaths := []string{"README.md", "bad.id/SKILL.md", "noskill"}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("issue paths = %v, want %v", gotPaths, wantPaths)
	}
}

func TestCorpusDigestIsDeterministicOverPathsAndContent(t *testing.T) {
	a := mustCorpus(t,
		file("xss/SKILL.md", "script"),
		file("sql/examples/payload.txt", "select"),
		file("sql/SKILL.md", "database"),
	)
	b := mustCorpus(t,
		file("sql/SKILL.md", "database"),
		file("sql/examples/payload.txt", "select"),
		file("xss/SKILL.md", "script"),
	)
	changed := mustCorpus(t,
		file("sql/SKILL.md", "database changed"),
		file("sql/examples/payload.txt", "select"),
		file("xss/SKILL.md", "script"),
	)

	if a.Digest() == "" || !strings.HasPrefix(a.Digest(), "sha256:") {
		t.Fatalf("Digest() = %q, want sha256:<hex>", a.Digest())
	}
	if a.Digest() != b.Digest() {
		t.Fatalf("digest changed with file order: %q != %q", a.Digest(), b.Digest())
	}
	if a.Digest() == changed.Digest() {
		t.Fatalf("digest did not change after content changed: %q", a.Digest())
	}
}

func TestValidIDUsesAWFStepIDRules(t *testing.T) {
	valid := []string{"sql", "xss_check", "_internal", "ssrf-audit"}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}

	invalid := []string{"", "1sql", "bad.id", "bad/id", "generate", "evaluate", "until", "then", "else", "body", "do", "catch", "finally"}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func file(path, content string) File {
	return File{Path: path, Content: []byte(content), Size: int64(len(content))}
}

func pathsOf(files []File) []string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths
}

func hasIssueKind(issues []Issue, kind IssueKind) bool {
	return slices.ContainsFunc(issues, func(issue Issue) bool {
		return issue.Kind == kind
	})
}

func issueKinds(issues []Issue) []IssueKind {
	kinds := make([]IssueKind, 0, len(issues))
	for _, issue := range issues {
		kinds = append(kinds, issue.Kind)
	}
	return kinds
}

func assertRouteIDs(t *testing.T, got []Selection, want ...string) {
	t.Helper()
	gotIDs := make([]string, 0, len(got))
	for _, sel := range got {
		if sel.Score <= 0 {
			t.Fatalf("selection %q has non-positive score %v", sel.ID, sel.Score)
		}
		gotIDs = append(gotIDs, sel.ID)
	}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("route ids = %v, want %v; selections = %#v", gotIDs, want, got)
	}
}

func mustCorpus(t *testing.T, files ...File) *Corpus {
	t.Helper()
	corpus, err := NewCorpus("test", files)
	if err != nil {
		t.Fatalf("NewCorpus error: %v", err)
	}
	return corpus
}

func containsToken(tokens []string, want string) bool {
	return slices.Contains(tokens, strings.ToLower(want))
}
