package skillroute

import "testing"

func TestBM25RetrievalRegressionFixture(t *testing.T) {
	corpus := mustCorpus(t,
		file("sql/SKILL.md", "SQL injection testing for unsafe database queries, UNION SELECT payloads, quote escaping, prepared statement bypasses."),
		file("sql/examples/payloads.txt", "' OR 1=1 --\nUNION SELECT password FROM users"),
		file("xss/SKILL.md", "Cross-site scripting review for reflected XSS, stored XSS, DOM sinks, script tags, HTML escaping, browser payloads."),
		file("xss/examples/payloads.txt", "<script>alert(1)</script>\n<img src=x onerror=alert(1)>"),
		file("ssrf/SKILL.md", "Server-side request forgery analysis for metadata service access, internal URLs, cloud credentials, redirect allowlists."),
		file("ssrf/examples/targets.txt", "http://169.254.169.254/latest/meta-data/\nhttp://localhost/admin"),
		file("auth/SKILL.md", "Authentication and authorization testing for session fixation, password reset, JWT validation, access control, privilege escalation."),
		file("auth/examples/checklist.txt", "reset token expiry\nrole based access control\nsession cookie flags"),
	)

	cases := []struct {
		query string
		want  string
	}{
		{"union select database quote", "sql"},
		{"prepared statement injection payload", "sql"},
		{"reflected script html escaping", "xss"},
		{"dom xss onerror browser", "xss"},
		{"metadata service cloud credentials", "ssrf"},
		{"internal localhost redirect allowlist", "ssrf"},
		{"password reset session token", "auth"},
		{"jwt authorization privilege escalation", "auth"},
	}

	var hitAt1, recallAt3 int
	var reciprocalRankSum float64
	for _, tc := range cases {
		got := corpus.Route(tc.query, 3)
		if len(got) > 0 && got[0].ID == tc.want {
			hitAt1++
		}
		for i, sel := range got {
			if sel.ID == tc.want {
				recallAt3++
				reciprocalRankSum += 1.0 / float64(i+1)
				break
			}
		}
	}

	if hitAt1 != len(cases) {
		t.Fatalf("Hit@1 = %d/%d, want all", hitAt1, len(cases))
	}
	if recallAt3 != len(cases) {
		t.Fatalf("Recall@3 = %d/%d, want all", recallAt3, len(cases))
	}
	if mrr := reciprocalRankSum / float64(len(cases)); mrr != 1.0 {
		t.Fatalf("MRR = %v, want 1.0", mrr)
	}
	if got := corpus.Route("kubernetes helm chart", 3); got != nil {
		t.Fatalf("unrelated query returned %#v, want nil", got)
	}
}

func TestBM25ScoreRegression(t *testing.T) {
	// Pins EXACT Route() scores for the standard fixture. Unlike
	// TestBM25RetrievalRegressionFixture (rank/ID only), this fails on any change
	// to the scoring formula (idf bm25.go:98, tf-saturation denom), tokenize,
	// weightedTerms, or textLike — even one that preserves ranking. If you change
	// the algorithm ON PURPOSE: recapture these literals AND bump RouterVersion
	// (corpus.go) — that pairing is the whole point.
	//
	// Exact == is intentional and cross-architecture-safe: Go enforces bit-identical
	// math.Log across arches for a fixed Go version (its own math.TestLog uses exact
	// equality), and the rest of the score is strict IEEE-754 float64 over a fixed
	// token order. Re-capture only if a Go *version* upgrade shifts the values.
	corpus := mustCorpus(t,
		file("sql/SKILL.md", "SQL injection testing for unsafe database queries, UNION SELECT payloads, quote escaping, prepared statement bypasses."),
		file("sql/examples/payloads.txt", "' OR 1=1 --\nUNION SELECT password FROM users"),
		file("xss/SKILL.md", "Cross-site scripting review for reflected XSS, stored XSS, DOM sinks, script tags, HTML escaping, browser payloads."),
		file("xss/examples/payloads.txt", "<script>alert(1)</script>\n<img src=x onerror=alert(1)>"),
		file("ssrf/SKILL.md", "Server-side request forgery analysis for metadata service access, internal URLs, cloud credentials, redirect allowlists."),
		file("ssrf/examples/targets.txt", "http://169.254.169.254/latest/meta-data/\nhttp://localhost/admin"),
		file("auth/SKILL.md", "Authentication and authorization testing for session fixation, password reset, JWT validation, access control, privilege escalation."),
		file("auth/examples/checklist.txt", "reset token expiry\nrole based access control\nsession cookie flags"),
	)
	cases := []struct {
		query string
		want  []Selection
	}{
		{"union select database quote", []Selection{{ID: "sql", Score: 8.40551871265491}}},
		{"reflected script html escaping", []Selection{{ID: "xss", Score: 7.369979651559467}, {ID: "sql", Score: 1.1819645499136429}}},
		{"metadata service cloud credentials", []Selection{{ID: "ssrf", Score: 8.162325818580282}}},
		{"password reset session token", []Selection{{ID: "auth", Score: 6.676140800154988}, {ID: "sql", Score: 0.7057736640811426}}},
	}
	for _, tc := range cases {
		got := corpus.Route(tc.query, 3)
		if len(got) != len(tc.want) {
			t.Fatalf("Route(%q) returned %d selections %#v, want %d", tc.query, len(got), got, len(tc.want))
		}
		for i, w := range tc.want {
			if got[i].ID != w.ID || got[i].Score != w.Score {
				t.Errorf("Route(%q)[%d] = {ID:%q Score:%v}, want {ID:%q Score:%v} — router algorithm changed; if intentional, recapture scores AND bump RouterVersion",
					tc.query, i, got[i].ID, got[i].Score, w.ID, w.Score)
			}
		}
	}
}
