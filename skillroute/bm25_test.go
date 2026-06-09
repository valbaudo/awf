package skillroute

import (
	"reflect"
	"testing"
)

func TestTokenizeLowercasesUnicodeLettersDigitsAndSplitsPunctuation(t *testing.T) {
	got := tokenize("SQL-injection, Café_123 <script>")
	want := []string{"sql", "injection", "café", "123", "script"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize() = %v, want %v", got, want)
	}
}

func TestRouteRanksByBM25ThenSkillIDAndDedupesQueryTerms(t *testing.T) {
	corpus := mustCorpus(t,
		file("sql/SKILL.md", "sql injection exploit database query database"),
		file("xss/SKILL.md", "xss script exploit browser render alert"),
		file("auth/SKILL.md", "authentication session password reset"),
	)

	once := corpus.Route("database", 10)
	repeated := corpus.Route("database database database", 10)
	assertRouteIDs(t, once, "sql")
	assertRouteIDs(t, repeated, "sql")
	if once[0].Score != repeated[0].Score {
		t.Fatalf("repeated query score = %v, want same as single term %v", repeated[0].Score, once[0].Score)
	}

	assertRouteIDs(t, corpus.Route("exploit", 10), "sql", "xss")
}

func TestRouteLimitAndZeroMatches(t *testing.T) {
	corpus := mustCorpus(t,
		file("sql/SKILL.md", "sql injection database"),
		file("xss/SKILL.md", "cross site scripting"),
	)

	if got := corpus.Route("sql", 0); got != nil {
		t.Fatalf("Route limit 0 = %#v, want nil", got)
	}
	if got := corpus.Route("kubernetes helm chart", 10); got != nil {
		t.Fatalf("Route unrelated query = %#v, want nil", got)
	}
	assertRouteIDs(t, corpus.Route("site sql", 1), "sql")
}

func TestWeightedFullSkillTextUsesSkillMDPathsAndNestedTextFiles(t *testing.T) {
	corpus := mustCorpus(t,
		file("sql/SKILL.md", "database query"),
		file("sql/examples/payload.txt", "' OR 1=1 UNION SELECT"),
		file("sql/bin/payload.bin", string([]byte{0, 1, 2, 3})),
		file("xss/SKILL.md", "browser payload script"),
	)

	sql := corpus.skills["sql"]
	for _, want := range []string{"sql", "skill", "md", "examples", "payload", "txt", "union", "select"} {
		if !containsToken(sql.Tokens, want) {
			t.Fatalf("sql weighted tokens missing %q: %v", want, sql.Tokens)
		}
	}
	if containsToken(sql.Tokens, "\x00") {
		t.Fatalf("binary nested file contributed tokens: %v", sql.Tokens)
	}

	assertRouteIDs(t, corpus.Route("union select", 10), "sql")
	assertRouteIDs(t, corpus.Route("examples payload", 10), "sql", "xss")
}

func TestRouterParams(t *testing.T) {
	got := RouterParams()
	want := map[string]float64{
		"k1":                1.2,
		"b":                 0.75,
		"skill_md_weight":   SkillMDWeight,
		"path_weight":       PathWeight,
		"text_file_weight":  TextFileWeight,
		"text_like_percent": TextLikePercent,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RouterParams() = %#v, want %#v", got, want)
	}
}
