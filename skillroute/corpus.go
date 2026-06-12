package skillroute

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	LayoutSkillDirs   = "skill_dirs"
	MaxSelectionLimit = 64

	RouterName = "bm25"
	// RouterVersion must be bumped whenever the scoring formula (bm25.go score/idf),
	// tokenization, weighting, the textLike threshold, or any RouterParams value changes.
	// TestBM25ScoreRegression (quality_test.go) pins exact scores so a formula change
	// is caught in CI, ensuring the bump is never forgotten.
	RouterVersion   = "bm25-weighted-v1"
	SkillMDWeight   = 4
	PathWeight      = 2
	TextFileWeight  = 1
	TextLikePercent = 85
)

type File struct {
	Path    string
	Content []byte
	Size    int64
	SHA256  string
}

type Corpus struct {
	skills   map[string]*Skill
	skillIDs []string
	digest   string
	index    bm25Index
}

type Skill struct {
	ID          string
	Files       []File
	Tokens      []string
	tokenFreq   map[string]int
	tokenLength int
}

type Selection struct {
	ID    string
	Score float64
}

type IssueKind string

type Issue struct {
	Kind    IssueKind
	Path    string
	SkillID string
	Message string
}

type IssuesError []Issue

const (
	IssueInvalidPath    IssueKind = "invalid_path"
	IssueRootFile       IssueKind = "root_file"
	IssueInvalidSkillID IssueKind = "invalid_skill_id"
	IssueMissingSkillMD IssueKind = "missing_skill_md"
)

var reservedSkillIDs = map[string]bool{
	"generate": true,
	"evaluate": true,
	"until":    true,
	"then":     true,
	"else":     true,
	"body":     true,
	"do":       true,
	"catch":    true,
	"finally":  true,
}

func (e IssuesError) Error() string {
	switch len(e) {
	case 0:
		return "skillroute: no issues"
	case 1:
		return "skillroute: " + e[0].Message
	default:
		return fmt.Sprintf("skillroute: %d corpus issues", len(e))
	}
}

func NewCorpus(files []File) (*Corpus, error) {
	if issues := ValidateFiles(files); len(issues) > 0 {
		return nil, IssuesError(issues)
	}

	skills := map[string]*Skill{}
	for _, f := range sortedFiles(files) {
		skillID, _, _ := strings.Cut(f.Path, "/")
		skill := skills[skillID]
		if skill == nil {
			skill = &Skill{ID: skillID}
			skills[skillID] = skill
		}
		skill.Files = append(skill.Files, cloneFile(f))
	}

	ids := make([]string, 0, len(skills))
	for id := range skills {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		skill := skills[id]
		sort.Slice(skill.Files, func(i, j int) bool {
			return skill.Files[i].Path < skill.Files[j].Path
		})
		skill.tokenFreq, skill.tokenLength = weightedTerms(skill.Files)
		skill.Tokens = sortedTermKeys(skill.tokenFreq)
	}

	corpus := &Corpus{
		skills:   skills,
		skillIDs: ids,
	}
	corpus.digest = digestSkills(ids, skills)
	corpus.index = newBM25Index(ids, skills)
	return corpus, nil
}

func ValidateFiles(files []File) []Issue {
	issues := []Issue{}
	skillIDs := map[string]bool{}
	hasSkillMD := map[string]bool{}

	for _, f := range sortedFiles(files) {
		if !validManifestPath(f.Path) {
			issues = append(issues, Issue{
				Kind:    IssueInvalidPath,
				Path:    f.Path,
				Message: fmt.Sprintf("path %q must be a clean relative forward-slash path", f.Path),
			})
			continue
		}

		skillID, rest, ok := strings.Cut(f.Path, "/")
		if !ok {
			issues = append(issues, Issue{
				Kind:    IssueRootFile,
				Path:    f.Path,
				Message: fmt.Sprintf("path %q is at corpus root; files must live under a skill directory", f.Path),
			})
			continue
		}
		if !ValidID(skillID) {
			issues = append(issues, Issue{
				Kind:    IssueInvalidSkillID,
				Path:    f.Path,
				SkillID: skillID,
				Message: fmt.Sprintf("skill id %q is not a valid AWF-style identifier", skillID),
			})
			continue
		}

		skillIDs[skillID] = true
		if rest == "SKILL.md" {
			hasSkillMD[skillID] = true
		}
	}

	ids := make([]string, 0, len(skillIDs))
	for id := range skillIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !hasSkillMD[id] {
			issues = append(issues, Issue{
				Kind:    IssueMissingSkillMD,
				Path:    id,
				SkillID: id,
				Message: fmt.Sprintf("skill %q is missing SKILL.md", id),
			})
		}
	}

	return issues
}

func ValidID(id string) bool {
	if id == "" || reservedSkillIDs[id] {
		return false
	}
	for i, r := range id {
		if r > 127 {
			return false
		}
		if i == 0 {
			if !isASCIILetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !isASCIILetter(r) && !isASCIIDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func (c *Corpus) SkillIDs() []string {
	if c == nil {
		return nil
	}
	ids := make([]string, len(c.skillIDs))
	copy(ids, c.skillIDs)
	return ids
}

func (c *Corpus) Digest() string {
	if c == nil {
		return ""
	}
	return c.digest
}

func (c *Corpus) StageFiles(skillID, into string) ([]File, bool) {
	if c == nil {
		return nil, false
	}
	skill := c.skills[skillID]
	if skill == nil {
		return nil, false
	}

	files := make([]File, 0, len(skill.Files))
	prefix := skillID + "/"
	for _, f := range skill.Files {
		rel := strings.TrimPrefix(f.Path, prefix)
		staged := cloneFile(f)
		staged.Path = path.Join(into, skillID, rel)
		files = append(files, staged)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, true
}

func RouterParams() map[string]float64 {
	return map[string]float64{
		"k1":                bm25K1,
		"b":                 bm25B,
		"skill_md_weight":   SkillMDWeight,
		"path_weight":       PathWeight,
		"text_file_weight":  TextFileWeight,
		"text_like_percent": TextLikePercent,
	}
}

func validManifestPath(p string) bool {
	if p == "" || p == "." || strings.Contains(p, "\\") || strings.ContainsRune(p, '\x00') || path.IsAbs(p) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func sortedFiles(files []File) []File {
	out := make([]File, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return bytes.Compare(out[i].Content, out[j].Content) < 0
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func cloneFile(f File) File {
	content := append([]byte(nil), f.Content...)
	sum := sha256.Sum256(content)
	return File{
		Path:    f.Path,
		Content: content,
		Size:    int64(len(content)),
		SHA256:  hex.EncodeToString(sum[:]),
	}
}

func digestSkills(ids []string, skills map[string]*Skill) string {
	h := sha256.New()
	writeHashString(h, "skillroute-corpus-v1\n")
	for _, id := range ids {
		skill := skills[id]
		writeHashString(h, "skill\n")
		writeHashString(h, strconv.Itoa(len(id)))
		writeHashString(h, "\n")
		writeHashString(h, id)
		writeHashString(h, "\n")
		for _, f := range skill.Files {
			writeHashString(h, "file\n")
			writeHashString(h, strconv.Itoa(len(f.Path)))
			writeHashString(h, "\n")
			writeHashString(h, f.Path)
			writeHashString(h, "\n")
			writeHashString(h, strconv.Itoa(len(f.Content)))
			writeHashString(h, "\n")
			writeHashBytes(h, f.Content)
			writeHashString(h, "\n")
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeHashString(h hash.Hash, s string) {
	writeHashBytes(h, []byte(s))
}

func writeHashBytes(h hash.Hash, b []byte) {
	if _, err := h.Write(b); err != nil {
		panic(err)
	}
}

func isASCIILetter(r rune) bool {
	return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
}

func isASCIIDigit(r rune) bool {
	return '0' <= r && r <= '9'
}
