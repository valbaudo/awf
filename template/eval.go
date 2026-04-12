package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MaxInlineBytes is the upper bound on a resolved ref's rendered/string size.
// Values exceeding this cap reject with AWF4001 at resolution — spec §7 / §4
// directs authors to pass large data via output_files (CAS-stored, referenced
// by name) instead of inlining it into expressions or run: commands.
//
// Phase 2 default: 64 KiB — matches ir.maxExpressionBytes for symmetry. The
// constant is exported so test fixtures and (eventually) a config layer can
// reason about it without parsing the package docs.
const MaxInlineBytes = 64 * 1024

// Evaluation error codes — the AWF4xxx namespace reserved by the Phase 2
// design spec for runtime evaluation errors. These mirror the AWF1xxx /
// AWF2xxx / AWF3xxx string-code shape ir/diagnostic.go uses for VALIDATION
// diagnostics, but they are NOT registered in ir.catalog because they arise
// at runtime — the interpreter (slice 2.5) catches them and projects them
// into node.failed events and OTel attrs, not into `awf validate` output.
const (
	EvalCodeOversize      = "AWF4001" // ref resolved to a value > MaxInlineBytes
	EvalCodeRefUnresolved = "AWF4002" // ref doesn't bind in the scope
	EvalCodeTypeMismatch  = "AWF4003" // operator's operand types don't match (no coercion per §7)
	EvalCodeInvalidScalar = "AWF4004" // a {{ }} slot resolved to a non-scalar (map/slice/nil)
	EvalCodeDeferred      = "AWF4099" // ref shape valid but resolution lands in a later slice (slice 2.4 for stdout, etc.)
)

// EvalError is the typed runtime error returned by Substitute / EvalBool. The
// Msg field includes any positional context as free text (e.g. "slot scan at
// offset N"); a structured Pos field is deferred until Phase 6's OTel
// projection has a consumer for it.
type EvalError struct {
	Code string
	Msg  string
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// Scope is the value source the evaluator and substituter consume. The single-
// method shape keeps the template package engine-free: engine.Scope (slice 2.3
// in engine/scope.go) implements it from *RunState + *ir.Workflow + a runtime
// context path. Test code can implement it from a flat map (see mapScope in
// eval_test.go) — the evaluator's mechanics are exercised in isolation that way.
//
// The returned value is the typed reference value — one of: string, bool,
// float64, int, json.Number, map[string]any, []any, nil. The size check
// (AWF4001) is the evaluator's responsibility, NOT the Scope's — the Scope
// just returns the value; the evaluator applies MaxInlineBytes after.
type Scope interface {
	Resolve(ref *Ref) (any, error)
}

// Substitute renders host by replacing every {{ ref }} slot with the typed
// value Scope.Resolve returns for that ref. The slot scanner is slice 1.3's
// Slots; ParseRef parses each slot's inner content (substitution targets are
// refs only per AWF §7, never full expressions). Returned errors are always
// *EvalError.
func Substitute(host string, scope Scope) (string, error) {
	slots, err := Slots(host)
	if err != nil {
		var se *SyntaxError
		if errors.As(err, &se) {
			return "", &EvalError{Code: EvalCodeRefUnresolved, Msg: fmt.Sprintf("slot scan at offset %d: %s", se.Pos, se.Msg)}
		}
		return "", &EvalError{Code: EvalCodeRefUnresolved, Msg: "slot scan: " + err.Error()}
	}
	if len(slots) == 0 {
		return host, nil
	}
	var b strings.Builder
	b.Grow(len(host))
	cursor := 0
	for _, sl := range slots {
		b.WriteString(host[cursor:sl.Start])
		ref, perr := ParseRef(strings.TrimSpace(sl.Inner))
		if perr != nil {
			var se *SyntaxError
			if errors.As(perr, &se) {
				hostPos := sl.Start + len(slotOpen) + se.Pos
				return "", &EvalError{Code: EvalCodeRefUnresolved, Msg: fmt.Sprintf("parse slot at host offset %d: %s", hostPos, se.Msg)}
			}
			return "", &EvalError{Code: EvalCodeRefUnresolved, Msg: "parse slot: " + perr.Error()}
		}
		v, rerr := resolveRefValue(scope, ref)
		if rerr != nil {
			var ee *EvalError
			if errors.As(rerr, &ee) {
				return "", &EvalError{Code: ee.Code, Msg: fmt.Sprintf("slot at host offset %d: %s", sl.Start, ee.Msg)}
			}
			return "", &EvalError{Code: EvalCodeRefUnresolved, Msg: fmt.Sprintf("slot at host offset %d: %s", sl.Start, rerr.Error())}
		}
		s, sErr := renderScalar(v)
		if sErr != nil {
			return "", &EvalError{Code: EvalCodeInvalidScalar, Msg: fmt.Sprintf("slot at host offset %d: %s", sl.Start, sErr.Error())}
		}
		b.WriteString(s)
		cursor = sl.End
	}
	b.WriteString(host[cursor:])
	return b.String(), nil
}

// EvalBool evaluates e against scope and returns its boolean value. The expr's
// top-level value MUST be a bool (no truthiness coercion); a non-bool top-level
// is AWF4003 (type mismatch). Short-circuiting: && stops on left-false; || on
// left-true.
func EvalBool(e Expr, scope Scope) (bool, error) {
	v, err := evalExpr(e, scope)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("top-level expression is %T, want bool", v)}
	}
	return b, nil
}

// evalExpr walks e and returns the typed value of the root node. Internal —
// the public API is EvalBool. The recursive walk threads errors and applies
// short-circuiting in && / ||.
func evalExpr(e Expr, scope Scope) (any, error) {
	switch v := e.(type) {
	case *BoolLit:
		return v.Value, nil
	case *StringLit:
		return v.Value, nil
	case *NumberLit:
		// json.Number → float64 (Phase 2 normalizes all numbers to float64; spec §7 says
		// no coercion across types but doesn't promise int/float distinction within numerics).
		f, err := v.Value.Float64()
		if err != nil {
			return nil, &EvalError{Code: EvalCodeInvalidScalar, Msg: fmt.Sprintf("number literal %q: %v", v.Value, err)}
		}
		return f, nil
	case *NullLit:
		return nil, nil
	case *Ref:
		return resolveRefValue(scope, v)
	case *NotExpr:
		x, err := evalExpr(v.X, scope)
		if err != nil {
			return nil, err
		}
		b, ok := x.(bool)
		if !ok {
			return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("! operand is %T, want bool", x)}
		}
		return !b, nil
	case *AndExpr:
		l, err := evalExpr(v.L, scope)
		if err != nil {
			return nil, err
		}
		lb, ok := l.(bool)
		if !ok {
			return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("&& left operand is %T, want bool", l)}
		}
		if !lb {
			return false, nil // short-circuit — right not evaluated
		}
		r, err := evalExpr(v.R, scope)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("&& right operand is %T, want bool", r)}
		}
		return rb, nil
	case *OrExpr:
		l, err := evalExpr(v.L, scope)
		if err != nil {
			return nil, err
		}
		lb, ok := l.(bool)
		if !ok {
			return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("|| left operand is %T, want bool", l)}
		}
		if lb {
			return true, nil // short-circuit
		}
		r, err := evalExpr(v.R, scope)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("|| right operand is %T, want bool", r)}
		}
		return rb, nil
	case *CmpExpr:
		l, err := evalExpr(v.L, scope)
		if err != nil {
			return nil, err
		}
		r, err := evalExpr(v.R, scope)
		if err != nil {
			return nil, err
		}
		return compare(v.Op, l, r)
	}
	return nil, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("unhandled expr node %T", e)}
}

// resolveRefValue calls scope.Resolve, then applies the MaxInlineBytes check.
// Centralized so EvalBool (via evalExpr → Ref) and Substitute share the size
// gate — the design spec says the limit is "at resolution," not per-operation.
func resolveRefValue(scope Scope, ref *Ref) (any, error) {
	v, err := scope.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if err := checkInlineSize(v); err != nil {
		return nil, err
	}
	return v, nil
}

func checkInlineSize(v any) error {
	var n int
	switch x := v.(type) {
	case []byte:
		n = len(x)
	case string:
		n = len(x)
	default:
		return nil // bool / number / nil / composite — all bounded or rejected elsewhere
	}
	if n > MaxInlineBytes {
		return &EvalError{
			Code: EvalCodeOversize,
			Msg:  fmt.Sprintf("ref value is %d bytes, MaxInlineBytes is %d (pass via output_files per spec §4)", n, MaxInlineBytes),
		}
	}
	return nil
}

// renderScalar converts a typed value to its substitution string form. Spec §7:
// "typed scalars rendered to string." Composites (map / slice) and nil are not
// renderable scalars and reject with AWF4004 (raised by Substitute) — silently
// rendering nil as "null" (Jinja2 default) could corrupt commands.
func renderScalar(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", fmt.Errorf("nil value not renderable as scalar — pass via output_files or guard with `if x == null` (spec §7)")
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case *int:
		if x == nil {
			return "", fmt.Errorf("nil *int not renderable as scalar")
		}
		return strconv.Itoa(*x), nil
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) && !math.IsNaN(x) {
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case json.Number:
		return string(x), nil
	}
	return "", fmt.Errorf("value of type %T is not a renderable scalar (use output_files for composite data)", v)
}

// compare implements the §7 comparison ops with NO type coercion EXCEPT for
// null (Design question 4): == / != with at least one nil operand return
// (l == nil && r == nil) and its negation; ordered ops on null still error.
// Numeric comparisons normalize both sides to float64 (json.Unmarshal of any
// number produces float64; literal NumberLit decodes to float64 here). String /
// string and bool / bool use Go-native ==. Ordered comparisons (< <= > >=) on
// non-numeric types are AWF4003.
func compare(op string, l, r any) (bool, error) {
	if l == nil || r == nil {
		if op == "==" {
			return l == nil && r == nil, nil
		}
		if op == "!=" {
			return l != nil || r != nil, nil
		}
		return false, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("%s with null operand requires == or !=", op)}
	}
	lf, lOK := toFloat(l)
	rf, rOK := toFloat(r)
	if lOK && rOK {
		switch op {
		case "==":
			return lf == rf, nil
		case "!=":
			return lf != rf, nil
		case "<":
			return lf < rf, nil
		case "<=":
			return lf <= rf, nil
		case ">":
			return lf > rf, nil
		case ">=":
			return lf >= rf, nil
		}
	}
	if op != "==" && op != "!=" {
		return false, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("%s requires numeric operands, got %T and %T", op, l, r)}
	}
	if !sameKind(l, r) {
		return false, &EvalError{Code: EvalCodeTypeMismatch, Msg: fmt.Sprintf("== / != across types: %T vs %T", l, r)}
	}
	eq := equalValue(l, r)
	if op == "==" {
		return eq, nil
	}
	return !eq, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case *int:
		if x == nil {
			return 0, false
		}
		return float64(*x), true
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func sameKind(l, r any) bool {
	switch l.(type) {
	case bool:
		_, ok := r.(bool)
		return ok
	case string:
		_, ok := r.(string)
		return ok
	case []byte:
		_, ok := r.([]byte)
		return ok
	}
	return false
}

func equalValue(l, r any) bool {
	switch x := l.(type) {
	case bool:
		return x == r.(bool)
	case string:
		return x == r.(string)
	case []byte:
		return string(x) == string(r.([]byte))
	}
	return false
}
