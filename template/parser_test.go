package template

import (
	"errors"
	"strings"
	"testing"
)

// dumpRef stringifies a ref to "a.b.c" form for terse test assertions.
func dumpRef(r *Ref) string {
	parts := make([]string, 0, len(r.Segments))
	for _, s := range r.Segments {
		if s.IsIndex {
			parts = append(parts, "<idx>")
		} else {
			parts = append(parts, s.Ident)
		}
	}
	return strings.Join(parts, ".")
}

func TestParseRefHappyPath(t *testing.T) {
	cases := []struct {
		src   string
		dump  string
		idxAt int // -1 if no integer segment
	}{
		{"run.id", "run.id", -1},
		{"input.cve_id", "input.cve_id", -1},
		{"step.triage.web_exploitable", "step.triage.web_exploitable", -1},
		{"step.cve-pipeline.field", "step.cve-pipeline.field", -1},
		{"a.0.b", "a.<idx>.b", 1},
		{"single", "single", -1},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			r, err := ParseRef(c.src)
			if err != nil {
				t.Fatal(err)
			}
			if got := dumpRef(r); got != c.dump {
				t.Errorf("ParseRef(%q) = %s, want %s", c.src, got, c.dump)
			}
			if c.idxAt >= 0 {
				if !r.Segments[c.idxAt].IsIndex {
					t.Errorf("segment[%d] is not an index", c.idxAt)
				}
				if r.Segments[c.idxAt].Index != 0 {
					t.Errorf("segment[%d].Index = %d, want 0", c.idxAt, r.Segments[c.idxAt].Index)
				}
			}
		})
	}
}

func TestParseRefErrors(t *testing.T) {
	cases := []struct {
		src    string
		msgSub string
	}{
		{"a.", "expected ident or integer"},
		{"a..b", "expected ident or integer"},
		{".a", "expected identifier"},
		{"a.-1", "non-negative"},
		{`"string"`, "expected identifier"},
		{"1.2", "expected identifier"},
		{"a b", "expected end of reference"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := ParseRef(c.src)
			if err == nil {
				t.Fatalf("expected error containing %q", c.msgSub)
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err is %T, want *SyntaxError: %v", err, err)
			}
			if !strings.Contains(se.Msg, c.msgSub) {
				t.Errorf("err.Msg = %q, want substring %q", se.Msg, c.msgSub)
			}
		})
	}
}

func TestParseExprHappyPath(t *testing.T) {
	// Smoke-test each production in the §B grammar at least once.
	cases := []string{
		// literals
		"true",
		"false",
		"null",
		"42",
		"-3.14",
		`"hello"`,
		// refs
		"run.id",
		"step.triage.web_exploitable",
		// comparisons
		"a == 1",
		"a != b",
		"a < b",
		"a <= b",
		"a > b",
		"a >= b",
		`"hello" == step.x.out`,
		// boolean
		"!a",
		"!!a",
		"a && b",
		"a || b",
		// precedence: && binds tighter than ||
		"a || b && c",
		// parens override
		"(a || b) && c",
		// negation around a paren
		"!(a && b)",
		// the standard's Appendix A condition
		"!(step.triage.web_exploitable && step.triage.has_source)",
		"evaluate.verified && evaluate.detections == 5 && evaluate.false_positives == 0",
		// chained boolean (left-associative)
		"a && b && c",
		"a || b || c",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			if _, err := ParseExpr(src); err != nil {
				t.Errorf("ParseExpr(%q): %v", src, err)
			}
		})
	}
}

func TestParseExprPrecedence(t *testing.T) {
	// `a || b && c` must parse as a || (b && c), so the top node is OrExpr.
	e, err := ParseExpr("a || b && c")
	if err != nil {
		t.Fatal(err)
	}
	or, ok := e.(*OrExpr)
	if !ok {
		t.Fatalf("top = %T, want *OrExpr", e)
	}
	if _, ok := or.R.(*AndExpr); !ok {
		t.Fatalf("OrExpr.R = %T, want *AndExpr", or.R)
	}
	if _, ok := or.L.(*Ref); !ok {
		t.Fatalf("OrExpr.L = %T, want *Ref", or.L)
	}
}

func TestParseExprNonAssociative(t *testing.T) {
	// Comparisons are non-associative — chaining is an error.
	for _, src := range []string{"a < b < c", "a == b == c", "a <= b > c"} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpr(src)
			if err == nil {
				t.Fatalf("expected non-associative error")
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err is %T, want *SyntaxError: %v", err, err)
			}
			if !strings.Contains(se.Msg, "non-associative") {
				t.Errorf("err.Msg = %q, want 'non-associative'", se.Msg)
			}
		})
	}
}

func TestParseExprUnaryBindsLooseThanCmp(t *testing.T) {
	// §B grammar: unary := "!" unary | comparison, so `!a == b` parses as `!(a == b)`,
	// NOT `(!a) == b`. This is surprising (most languages bind `!` tighter than `==`)
	// — locked here so we notice if the grammar ever shifts. The expected tree is
	// NotExpr{CmpExpr{==, Ref(a), Ref(b)}}; the OTHER tree (CmpExpr at top) would mean
	// `!` had been re-bound tighter than comparison.
	e, err := ParseExpr("!a == b")
	if err != nil {
		t.Fatal(err)
	}
	not, ok := e.(*NotExpr)
	if !ok {
		t.Fatalf("top = %T, want *NotExpr (`!` wraps the whole comparison)", e)
	}
	cmp, ok := not.X.(*CmpExpr)
	if !ok {
		t.Fatalf("NotExpr.X = %T, want *CmpExpr", not.X)
	}
	if cmp.Op != "==" {
		t.Errorf("CmpExpr.Op = %q, want '=='", cmp.Op)
	}
	if _, ok := cmp.L.(*Ref); !ok {
		t.Errorf("CmpExpr.L = %T, want *Ref(a)", cmp.L)
	}
	if _, ok := cmp.R.(*Ref); !ok {
		t.Errorf("CmpExpr.R = %T, want *Ref(b)", cmp.R)
	}
}

func TestParseExprErrors(t *testing.T) {
	cases := []struct {
		src    string
		msgSub string
	}{
		{"1 + 2", "unexpected character"}, // no arithmetic (lexer rejects '+')
		{"foo()", "unexpected"},           // no function calls
		{"(a == b", "expected ')'"},       // unclosed paren
		{")", "expected primary"},         // stray close paren
		{"&&", "expected primary"},        // operator without LHS
		{"a &&", "expected primary"},      // operator without RHS
		{"", "expected primary"},          // empty input is not an expression
		{"a b", "unexpected"},             // trailing garbage
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := ParseExpr(c.src)
			if err == nil {
				t.Fatalf("expected error containing %q", c.msgSub)
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err is %T, want *SyntaxError: %v", err, err)
			}
			if !strings.Contains(se.Msg, c.msgSub) {
				t.Errorf("err.Msg = %q, want substring %q", se.Msg, c.msgSub)
			}
		})
	}
}

func TestParseExprErrorMessagesUseEOFForEOF(t *testing.T) {
	// EOF in the error message renders as "EOF", not as an empty-string `""` — M7 fix.
	for _, src := range []string{"(a", ""} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpr(src)
			if err == nil {
				t.Fatalf("expected error")
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err is %T, want *SyntaxError", err)
			}
			if strings.Contains(se.Msg, `""`) {
				t.Errorf("err.Msg should not render EOF as empty string: %q", se.Msg)
			}
		})
	}
}
