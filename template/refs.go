package template

// References walks e and returns every Ref encountered, in left-to-right source order.
// Duplicates are preserved (the validator can dedupe by ref path if it wants — slice 1.4 will).
// The empty slice is returned for an Expr that contains no refs (a literal-only condition).
//
// Used by validation to compute the set of references the workflow makes, feeding the
// "output_schema iff referenced" check (AWF3001/AWF3002) and the structural rule that bans
// references to undeclared steps / unbound names.
func References(e Expr) []Ref {
	var out []Ref
	collectRefs(e, &out)
	return out
}

func collectRefs(e Expr, out *[]Ref) {
	switch v := e.(type) {
	case *OrExpr:
		collectRefs(v.L, out)
		collectRefs(v.R, out)
	case *AndExpr:
		collectRefs(v.L, out)
		collectRefs(v.R, out)
	case *NotExpr:
		collectRefs(v.X, out)
	case *CmpExpr:
		collectRefs(v.L, out)
		collectRefs(v.R, out)
	case *Ref:
		*out = append(*out, *v)
	case *StringLit, *NumberLit, *BoolLit, *NullLit:
		// no refs
	}
}
