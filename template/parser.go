package template

import (
	"encoding/json"
	"fmt"
)

// tokDesc renders a token for use in error messages: "EOF" for TEOF (the Text field is empty
// there, which would otherwise render as `got ""`), and the %q-quoted text for everything else.
func tokDesc(t Token) string {
	if t.Kind == TEOF {
		return "EOF"
	}
	return fmt.Sprintf("%q", t.Text)
}

// ParseExpr parses src as a full §B expression. src is the inner content (callers strip any
// `{{ … }}` envelope themselves — see Slots). Returns a *SyntaxError on any deviation from the
// grammar; the input must be fully consumed.
func ParseExpr(src string) (Expr, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TEOF {
		return nil, &SyntaxError{Pos: p.peek().Pos, Msg: fmt.Sprintf("unexpected %s after expression", tokDesc(p.peek()))}
	}
	return e, nil
}

// ParseRef parses src as a single reference (substitution slot contents). The input must be
// exactly one ref — no operators, no parens, no literals, and no trailing tokens.
func ParseRef(src string) (*Ref, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	r, err := p.parseRef()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TEOF {
		return nil, &SyntaxError{Pos: p.peek().Pos, Msg: fmt.Sprintf("expected end of reference, got %s", tokDesc(p.peek()))}
	}
	return r, nil
}

// parser is the recursive-descent state. Internal — callers use ParseExpr / ParseRef.
type parser struct {
	toks []Token
	i    int
}

func (p *parser) peek() Token { return p.toks[p.i] }

func (p *parser) consume() Token {
	t := p.toks[p.i]
	p.i++
	return t
}

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TOr {
		p.consume()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &OrExpr{L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TAnd {
		p.consume()
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &AndExpr{L: l, R: r}
	}
	return l, nil
}

// parseUnary parses `unary := "!" unary | comparison`. Note that `!` binds LOOSER than the
// comparison operator: `!a == b` parses as `!(a == b)`, NOT `(!a) == b`. This is the §B grammar's
// choice (the operand of `!` is itself a unary, which expands through comparison). Author-facing
// warnings for the unparenthesized form belong in slice 1.4 (validator), not here.
func (p *parser) parseUnary() (Expr, error) {
	if p.peek().Kind == TNot {
		p.consume()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &NotExpr{X: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Expr, error) {
	l, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if op, ok := cmpOpString(p.peek().Kind); ok {
		p.consume()
		r, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		// Non-associativity (§B): a < b < c is invalid.
		if _, ok := cmpOpString(p.peek().Kind); ok {
			return nil, &SyntaxError{Pos: p.peek().Pos, Msg: "comparisons are non-associative (a<b<c is invalid)"}
		}
		return &CmpExpr{Op: op, L: l, R: r}, nil
	}
	return l, nil
}

func cmpOpString(k TokenKind) (string, bool) {
	switch k {
	case TEq:
		return "==", true
	case TNeq:
		return "!=", true
	case TLt:
		return "<", true
	case TLe:
		return "<=", true
	case TGt:
		return ">", true
	case TGe:
		return ">=", true
	}
	return "", false
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TLParen:
		p.consume()
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != TRParen {
			return nil, &SyntaxError{Pos: p.peek().Pos, Msg: fmt.Sprintf("expected ')', got %s", tokDesc(p.peek()))}
		}
		p.consume()
		return e, nil
	case TNumber:
		p.consume()
		return &NumberLit{Value: json.Number(t.Text)}, nil
	case TString:
		p.consume()
		return &StringLit{Value: t.Text}, nil
	case TIdent:
		switch t.Text {
		case "true":
			p.consume()
			return &BoolLit{Value: true}, nil
		case "false":
			p.consume()
			return &BoolLit{Value: false}, nil
		case "null":
			p.consume()
			return &NullLit{}, nil
		}
		return p.parseRef()
	}
	return nil, &SyntaxError{Pos: t.Pos, Msg: fmt.Sprintf("expected primary, got %s", tokDesc(t))}
}

func (p *parser) parseRef() (*Ref, error) {
	first := p.peek()
	if first.Kind != TIdent {
		return nil, &SyntaxError{Pos: first.Pos, Msg: fmt.Sprintf("expected identifier, got %s", tokDesc(first))}
	}
	p.consume()
	r := &Ref{Segments: []Segment{{Ident: first.Text, Pos: first.Pos}}}
	for p.peek().Kind == TDot {
		p.consume()
		t := p.peek()
		switch t.Kind {
		case TIdent:
			p.consume()
			r.Segments = append(r.Segments, Segment{Ident: t.Text, Pos: t.Pos})
		case TNumber:
			p.consume()
			n, err := (json.Number(t.Text)).Int64()
			if err != nil || n < 0 {
				return nil, &SyntaxError{Pos: t.Pos, Msg: fmt.Sprintf("ref index must be a non-negative integer, got %q", t.Text)}
			}
			r.Segments = append(r.Segments, Segment{Index: int(n), IsIndex: true, Pos: t.Pos})
		default:
			return nil, &SyntaxError{Pos: t.Pos, Msg: fmt.Sprintf("expected ident or integer after '.', got %s", tokDesc(t))}
		}
	}
	return r, nil
}
