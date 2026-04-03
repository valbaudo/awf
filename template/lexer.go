// Package template implements the §7 mini-language: lexer + recursive-descent parser for the
// bounded expression grammar from the Phase 1 design spec §B, plus reference extraction over
// the resulting AST and a `{{ … }}` slot scanner for substitution-bearing host strings.
//
// Evaluation (value substitution + bounded boolean evaluation) lands later — it needs state.
// This slice is state-free.
//
// The grammar:
//
//	expr       := or
//	or         := and ( "||" and )*
//	and        := unary ( "&&" unary )*
//	unary      := "!" unary | comparison
//	comparison := primary ( ("=="|"!="|"<"|"<="|">"|">=") primary )?   ; non-associative
//	primary    := ref | literal | "(" expr ")"
//	ref        := IDENT ( "." IDENT | "." INT )*
//	literal    := STRING | NUMBER | "true" | "false" | "null"
//
// No arithmetic, no function calls, no loops. Comparisons are non-associative (a<b<c invalid).
// Identifiers are [A-Za-z_][A-Za-z0-9_\-]* (hyphens allowed because step/binding ids may
// contain them in AWF, e.g. `cve-pipeline`).
package template

import (
	"fmt"
	"strings"
	"unicode"
)

// SyntaxError is the typed error returned by Lex / ParseExpr / ParseRef on any deviation from
// the §B grammar. Pos is the byte offset into the source; Msg is a short, position-free message.
// Callers extract Pos programmatically via errors.As(err, &se) — avoids the substring-match-on-
// the-rendered-message fragility we hit elsewhere.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("position %d: %s", e.Pos, e.Msg) }

// TokenKind enumerates the token kinds produced by Lex. The exhaustive switch in the parser is
// enforced by go-check-sumtype / exhaustive linters (configured at the module level).
type TokenKind int

const (
	TEOF TokenKind = iota
	TIdent
	TNumber
	TString
	TDot
	TLParen
	TRParen
	TOr  // ||
	TAnd // &&
	TNot // !
	TEq  // ==
	TNeq // !=
	TLt  // <
	TLe  // <=
	TGt  // >
	TGe  // >=
)

func (k TokenKind) String() string {
	switch k {
	case TEOF:
		return "EOF"
	case TIdent:
		return "IDENT"
	case TNumber:
		return "NUMBER"
	case TString:
		return "STRING"
	case TDot:
		return "."
	case TLParen:
		return "("
	case TRParen:
		return ")"
	case TOr:
		return "||"
	case TAnd:
		return "&&"
	case TNot:
		return "!"
	case TEq:
		return "=="
	case TNeq:
		return "!="
	case TLt:
		return "<"
	case TLe:
		return "<="
	case TGt:
		return ">"
	case TGe:
		return ">="
	}
	return fmt.Sprintf("TokenKind(%d)", k)
}

// Token is one lexed token. Pos is the byte offset into the source.
type Token struct {
	Kind TokenKind
	Text string // for TIdent: the identifier; for TNumber: the raw number literal (may include leading '-' and decimal point); for TString: decoded content
	Pos  int
}

// Lex tokenizes src per the §B grammar. Whitespace is skipped between tokens.
func Lex(src string) ([]Token, error) {
	var out []Token
	i := 0
	for i < len(src) {
		c := src[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		start := i
		switch {
		case c == '(':
			out = append(out, Token{TLParen, "(", start})
			i++
		case c == ')':
			out = append(out, Token{TRParen, ")", start})
			i++
		case c == '.':
			out = append(out, Token{TDot, ".", start})
			i++
		case c == '|' && i+1 < len(src) && src[i+1] == '|':
			out = append(out, Token{TOr, "||", start})
			i += 2
		case c == '&' && i+1 < len(src) && src[i+1] == '&':
			out = append(out, Token{TAnd, "&&", start})
			i += 2
		case c == '!':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{TNeq, "!=", start})
				i += 2
			} else {
				out = append(out, Token{TNot, "!", start})
				i++
			}
		case c == '=':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{TEq, "==", start})
				i += 2
			} else {
				return nil, &SyntaxError{Pos: start, Msg: "bare '=' (use '==' for equality)"}
			}
		case c == '<':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{TLe, "<=", start})
				i += 2
			} else {
				out = append(out, Token{TLt, "<", start})
				i++
			}
		case c == '>':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{TGe, ">=", start})
				i += 2
			} else {
				out = append(out, Token{TGt, ">", start})
				i++
			}
		case c == '"':
			t, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
			i = next
		case c == '-':
			// `-` is only valid as a negative-number prefix (the grammar has no arithmetic).
			// A bare `-` or `-X` where X is not a digit would otherwise produce a malformed
			// NUMBER token whose downstream parse fails far from the source — reject it here.
			if i+1 >= len(src) || !isDigit(src[i+1]) {
				return nil, &SyntaxError{Pos: start, Msg: "unexpected '-' (only allowed as a negative-number prefix)"}
			}
			t, next := lexNumber(src, i)
			out = append(out, t)
			i = next
		case isDigit(c):
			t, next := lexNumber(src, i)
			out = append(out, t)
			i = next
		case isIdentStart(c):
			t, next := lexIdent(src, i)
			out = append(out, t)
			i = next
		default:
			return nil, &SyntaxError{Pos: start, Msg: fmt.Sprintf("unexpected character %q", string(c))}
		}
	}
	out = append(out, Token{TEOF, "", len(src)})
	return out, nil
}

// lexString scans a double-quoted string. Escape behavior is deliberately minimal: \" produces
// a literal ", \\ produces a literal \, and any other \X produces a literal X (so \n is the
// letter "n", NOT a newline). Templates rarely need non-printable characters in string
// literals; full JSON-like escapes can be added without breaking compatibility if a real use
// case appears.
func lexString(src string, start int) (Token, int, error) {
	// Consume the opening quote.
	i := start + 1
	var sb strings.Builder
	for i < len(src) && src[i] != '"' {
		if src[i] == '\\' && i+1 < len(src) {
			sb.WriteByte(src[i+1])
			i += 2
			continue
		}
		sb.WriteByte(src[i])
		i++
	}
	if i >= len(src) {
		return Token{}, 0, &SyntaxError{Pos: start, Msg: "unterminated string"}
	}
	return Token{TString, sb.String(), start}, i + 1, nil
}

func lexNumber(src string, start int) (Token, int) {
	j := start
	if src[j] == '-' {
		j++
	}
	for j < len(src) && isDigit(src[j]) {
		j++
	}
	// Consume "." only if followed by a digit — otherwise the dot is a ref-segment separator
	// (a.0.b parses as IDENT . NUMBER . IDENT, not IDENT . NUMBER(0.) IDENT).
	if j+1 < len(src) && src[j] == '.' && isDigit(src[j+1]) {
		j++
		for j < len(src) && isDigit(src[j]) {
			j++
		}
	}
	return Token{TNumber, src[start:j], start}, j
}

func lexIdent(src string, start int) (Token, int) {
	j := start
	for j < len(src) && isIdentCont(src[j]) {
		j++
	}
	return Token{TIdent, src[start:j], start}, j
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' }
func isIdentCont(c byte) bool  { return isIdentStart(c) || isDigit(c) || c == '-' }
