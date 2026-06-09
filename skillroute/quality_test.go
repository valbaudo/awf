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
