package template

import "encoding/json"

// Expr is the AST sum type for parsed condition expressions. Concrete kinds: *OrExpr, *AndExpr,
// *NotExpr, *CmpExpr, *Ref, *StringLit, *NumberLit, *BoolLit, *NullLit. Sealed by exprMarker —
// linters (go-check-sumtype / exhaustive) enforce exhaustive switches in consumers.
type Expr interface {
	exprMarker()
}

// OrExpr models `L || R` (left-associative chain via nested OrExpr).
type OrExpr struct {
	L, R Expr
}

// AndExpr models `L && R` (left-associative chain via nested AndExpr).
type AndExpr struct {
	L, R Expr
}

// NotExpr models `!X` (right-associative via nesting: `!!a` → NotExpr{NotExpr{Ref a}}).
type NotExpr struct {
	X Expr
}

// CmpExpr models a single non-associative comparison `L <op> R`. Op is one of
// "==", "!=", "<", "<=", ">", ">=".
type CmpExpr struct {
	Op   string
	L, R Expr
}

// Ref is a reference path: IDENT ( "." IDENT | "." INT )*. First segment is always an ident.
// Use Segments[0].Pos for "start of ref" diagnostics; per-segment Pos enables pointing at the
// specific failing segment (e.g. "step `triage` not declared" → Segments[1].Pos).
type Ref struct {
	Segments []Segment
}

// Segment is one ref segment. Exactly one of {Ident set, IsIndex true} holds; the other is zero.
// Pos is the byte offset of this segment's token in the source — preserved so validation can
// point at the specific failing segment (e.g. `step.triage.field` → "triage" unknown at pos 5).
type Segment struct {
	Ident   string // populated when IsIndex is false
	Index   int    // populated when IsIndex is true (non-negative integer)
	IsIndex bool
	Pos     int
}

// StringLit, NumberLit, BoolLit, NullLit cover the literal production. NumberLit holds the raw
// token text as a json.Number so the evaluator (Phase 2) can choose int/float decoding without
// loss; in this slice nothing decodes the value, it just round-trips the source.
type StringLit struct{ Value string }
type NumberLit struct{ Value json.Number }
type BoolLit struct{ Value bool }
type NullLit struct{}

func (*OrExpr) exprMarker()    {}
func (*AndExpr) exprMarker()   {}
func (*NotExpr) exprMarker()   {}
func (*CmpExpr) exprMarker()   {}
func (*Ref) exprMarker()       {}
func (*StringLit) exprMarker() {}
func (*NumberLit) exprMarker() {}
func (*BoolLit) exprMarker()   {}
func (*NullLit) exprMarker()   {}
