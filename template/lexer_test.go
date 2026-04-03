package template

import (
	"errors"
	"strings"
	"testing"
)

func TestLexHappyPath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []TokenKind
	}{
		{"single ident", "foo", []TokenKind{TIdent, TEOF}},
		{"dotted ref", "step.triage.web_exploitable", []TokenKind{TIdent, TDot, TIdent, TDot, TIdent, TEOF}},
		{"hyphen in ident", "step.cve-pipeline.field", []TokenKind{TIdent, TDot, TIdent, TDot, TIdent, TEOF}},
		{"underscore start", "_internal", []TokenKind{TIdent, TEOF}},
		{"underscore in middle", "step._private.field", []TokenKind{TIdent, TDot, TIdent, TDot, TIdent, TEOF}},
		{"int segment", "a.0.b", []TokenKind{TIdent, TDot, TNumber, TDot, TIdent, TEOF}},
		{"comparison", "a == 1", []TokenKind{TIdent, TEq, TNumber, TEOF}},
		{"booleans", "true && false || null", []TokenKind{TIdent, TAnd, TIdent, TOr, TIdent, TEOF}},
		{"unary not", "!a", []TokenKind{TNot, TIdent, TEOF}},
		{"all comparison ops", "1 != 2 < 3 <= 4 > 5 >= 6", []TokenKind{TNumber, TNeq, TNumber, TLt, TNumber, TLe, TNumber, TGt, TNumber, TGe, TNumber, TEOF}},
		{"parens", "(a)", []TokenKind{TLParen, TIdent, TRParen, TEOF}},
		{"string literal", `"hello"`, []TokenKind{TString, TEOF}},
		{"string with escape", `"a\"b"`, []TokenKind{TString, TEOF}},
		{"negative number", "-5", []TokenKind{TNumber, TEOF}},
		{"float", "3.14", []TokenKind{TNumber, TEOF}},
		{"whitespace mix", "  a   ==  \t b\n", []TokenKind{TIdent, TEq, TIdent, TEOF}},
		{"empty input", "", []TokenKind{TEOF}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toks, err := Lex(c.src)
			if err != nil {
				t.Fatalf("Lex(%q) err = %v", c.src, err)
			}
			if len(toks) != len(c.want) {
				t.Fatalf("Lex(%q) produced %d tokens, want %d: %+v", c.src, len(toks), len(c.want), toks)
			}
			for i, k := range c.want {
				if toks[i].Kind != k {
					t.Errorf("token[%d].Kind = %v, want %v (full: %+v)", i, toks[i].Kind, k, toks)
				}
			}
		})
	}
}

func TestLexStringEscapes(t *testing.T) {
	toks, err := Lex(`"a\"b\\c"`)
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].Kind != TString {
		t.Fatalf("kind = %v, want TString", toks[0].Kind)
	}
	if toks[0].Text != `a"b\c` {
		t.Errorf("Text = %q, want %q", toks[0].Text, `a"b\c`)
	}
}

func TestLexNumberFollowedByRefDot(t *testing.T) {
	// `a.0.b` — the `0` is a ref-segment integer, not the start of a float `0.b`.
	// The lexer must NOT consume the dot after the digit unless a digit follows.
	toks, err := Lex("a.0.b")
	if err != nil {
		t.Fatal(err)
	}
	if toks[2].Kind != TNumber || toks[2].Text != "0" {
		t.Fatalf("token[2] = %+v, want TNumber \"0\"", toks[2])
	}
	if toks[3].Kind != TDot {
		t.Fatalf("token[3] = %+v, want TDot", toks[3])
	}
}

func TestLexPosition(t *testing.T) {
	toks, _ := Lex("a == 1")
	if toks[0].Pos != 0 || toks[1].Pos != 2 || toks[2].Pos != 5 {
		t.Errorf("positions = %d,%d,%d; want 0,2,5", toks[0].Pos, toks[1].Pos, toks[2].Pos)
	}
}

func TestLexErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantPos int
		msgSub  string
	}{
		{"=", 0, "bare '='"},
		{`"unterminated`, 0, "unterminated string"},
		{"@", 0, "unexpected character"},
		{"a + b", 2, "unexpected character"},
		// C1: bare `-` and `-X` where X is not a digit are rejected up front.
		{"-", 0, "only allowed as a negative-number prefix"},
		{"-foo", 0, "only allowed as a negative-number prefix"},
		{"--5", 0, "only allowed as a negative-number prefix"},
		{"5-", 1, "only allowed as a negative-number prefix"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := Lex(c.src)
			if err == nil {
				t.Fatalf("Lex(%q) expected error, got nil", c.src)
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("Lex(%q) err is %T, want *SyntaxError: %v", c.src, err, err)
			}
			if se.Pos != c.wantPos {
				t.Errorf("Lex(%q) err.Pos = %d, want %d (msg: %q)", c.src, se.Pos, c.wantPos, se.Msg)
			}
			if !strings.Contains(se.Msg, c.msgSub) {
				t.Errorf("Lex(%q) err.Msg = %q, want substring %q", c.src, se.Msg, c.msgSub)
			}
		})
	}
}
