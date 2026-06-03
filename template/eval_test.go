package template

import (
	"errors"
	"strings"
	"testing"
)

// mapScope is a test-only Scope that resolves refs against a flat map keyed by
// the dotted ref string ("run.id", "input.cve_id", "step.triage.exit_code", ...).
// Production callers MUST use engine.NewScope — mapScope deliberately knows
// nothing about AWF's loop-iter rules / IR step index (those are engine.Scope's
// job and are tested in engine/scope_test.go).
type mapScope map[string]any

func (m mapScope) Resolve(r *Ref) (any, error) {
	key := refKey(r)
	v, ok := m[key]
	if !ok {
		return nil, &EvalError{Code: EvalCodeRefUnresolved, Msg: "ref " + key + " not in scope"}
	}
	return v, nil
}

// refKey stringifies a Ref to dotted form ("a.b.c") for mapScope lookups. Index
// segments render as their integer ("a.0.b").
func refKey(r *Ref) string {
	parts := make([]string, 0, len(r.Segments))
	for _, s := range r.Segments {
		if s.IsIndex {
			parts = append(parts, "0") // tests don't use non-zero indices — keep it simple
			continue
		}
		parts = append(parts, s.Ident)
	}
	return strings.Join(parts, ".")
}

func TestSubstituteHappyPath(t *testing.T) {
	scope := mapScope{
		"run.id":                "run-abc123",
		"input.cve_id":          "CVE-2024-9999",
		"step.triage.exit_code": 0,
		"step.triage.message":   "hello world",
		"step.triage.verified":  true,
		"step.triage.count":     5.0, // json.Unmarshal of "5" → float64
	}
	cases := []struct {
		name string
		host string
		want string
	}{
		{"single ref", "{{ run.id }}", "run-abc123"},
		{"suffix concat", "{{ run.id }}:pr", "run-abc123:pr"},
		{"multiple slots", `./scan.sh "{{ input.cve_id }}" --run {{ run.id }}`, `./scan.sh "CVE-2024-9999" --run run-abc123`},
		{"integer renders without trailing zeros", "exit={{ step.triage.exit_code }}", "exit=0"},
		{"string output renders verbatim", "got: {{ step.triage.message }}", "got: hello world"},
		{"bool renders as true/false", "v={{ step.triage.verified }}", "v=true"},
		{"float renders compactly", "n={{ step.triage.count }}", "n=5"},
		{"no slots", "literal text", "literal text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Substitute(c.host, scope)
			if err != nil {
				t.Fatalf("Substitute(%q): %v", c.host, err)
			}
			if got != c.want {
				t.Errorf("Substitute(%q) = %q, want %q", c.host, got, c.want)
			}
		})
	}
}

func TestSubstituteEscapedBrace(t *testing.T) {
	// Host-level escape: `\{{` renders to a literal `{{` with the backslash consumed,
	// uniformly across any Template field. The escaped region is NOT a slot, so its
	// inner text (even an SSTI payload like `7*7`) never reaches ParseRef / the scope.
	// `\` is special ONLY immediately before `{{` — a lone `\` elsewhere is literal.
	scope := mapScope{"run.id": "run-abc123", "step.x.field": "RESOLVED"}
	cases := []struct {
		name string
		host string
		want string
	}{
		{"ssti payload round-trips", `scan \{{7*7}} now`, "scan {{7*7}} now"},
		{"escaped ref is not resolved", `\{{ step.x.field }}`, "{{ step.x.field }}"},
		{"escape mixed with a live slot", `lit \{{x}} live {{ run.id }}`, "lit {{x}} live run-abc123"},
		{"bare backslash elsewhere is literal", `a\b \{{x}}`, `a\b {{x}}`},
		{"backslash not before opener is literal", `path\to\file`, `path\to\file`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Substitute(c.host, scope)
			if err != nil {
				t.Fatalf("Substitute(%q): %v", c.host, err)
			}
			if got != c.want {
				t.Errorf("Substitute(%q) = %q, want %q", c.host, got, c.want)
			}
		})
	}
}

func TestSubstituteUnescapedBraceStillErrors(t *testing.T) {
	// An UNescaped `{{7*7}}` is unchanged behavior: it is parsed as a slot, ParseRef
	// rejects `7*7`, and Substitute returns AWF4005 (syntax). The escape must not
	// loosen this.
	_, err := Substitute("{{7*7}}", mapScope{})
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError: %v", err, err)
	}
	if ee.Code != EvalCodeSyntax {
		t.Errorf("err.Code = %q, want %q (AWF4005)", ee.Code, EvalCodeSyntax)
	}
}

func TestSubstituteUnresolvedRefIsAWF4002(t *testing.T) {
	_, err := Substitute("{{ step.ghost.field }}", mapScope{})
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError: %v", err, err)
	}
	if ee.Code != EvalCodeRefUnresolved {
		t.Errorf("err.Code = %q, want %q", ee.Code, EvalCodeRefUnresolved)
	}
}

func TestSubstituteSyntaxErrorIsAWF4005(t *testing.T) {
	// Host-template syntax errors — unterminated {{, nested {{, empty slot
	// inner — are AWF4005 (syntax), distinct from AWF4002 (resolution
	// failure). The ref never reached the scope.
	cases := []string{
		"{{ ",           // unterminated slot
		"{{ {{ a }} }}", // nested {{
		"{{}}",          // empty inner — ParseRef rejects
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Substitute(src, mapScope{})
			var ee *EvalError
			if !errors.As(err, &ee) {
				t.Fatalf("err is %T, want *EvalError: %v", err, err)
			}
			if ee.Code != EvalCodeSyntax {
				t.Errorf("err.Code = %q, want %q (AWF4005)", ee.Code, EvalCodeSyntax)
			}
		})
	}
}

func TestSubstituteSyntaxErrorHostOffsetIncludesSlotWhitespace(t *testing.T) {
	// Slot inner whitespace counts toward host offsets — ParseRef receives
	// the un-trimmed inner so its reported position is slot-local, and
	// Substitute translates via sl.Start + len(slotOpen) + se.Pos. A leading-
	// whitespace slot must NOT silently shift the reported position.
	host := "prefix {{   bad@token }} suffix"
	_, err := Substitute(host, mapScope{})
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError: %v", err, err)
	}
	if ee.Code != EvalCodeSyntax {
		t.Errorf("err.Code = %q, want %q", ee.Code, EvalCodeSyntax)
	}
	// The '@' lives at byte 15 of the host: "prefix " (7) + "{{" (2) + "   bad" (6) = 15.
	// The Msg should contain "host offset 15" — not 12 (which is what TrimSpace
	// would produce by stripping the 3 leading spaces from the slot inner before
	// ParseRef ran).
	if !strings.Contains(ee.Msg, "host offset 15") {
		t.Errorf("err.Msg = %q, want mention of host offset 15 (TrimSpace regression?)", ee.Msg)
	}
}

func TestSubstituteOversizeIsAWF4001(t *testing.T) {
	// A typed output value larger than MaxInlineBytes must reject at resolution.
	// (Using a string-typed output here since step.<id>.stdout is deferred to
	// slice 2.4 — see Design question 2.)
	huge := strings.Repeat("a", MaxInlineBytes+1)
	scope := mapScope{"step.dump.payload": huge}
	_, err := Substitute("{{ step.dump.payload }}", scope)
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError: %v", err, err)
	}
	if ee.Code != EvalCodeOversize {
		t.Errorf("err.Code = %q, want %q (AWF4001)", ee.Code, EvalCodeOversize)
	}
}

func TestSubstituteNilIsAWF4004(t *testing.T) {
	// Per spec §7, typed scalars render to string. nil is not a scalar — and
	// silently rendering "null" (Jinja2 / Go default) would corrupt commands
	// (e.g., ./scan null instead of ./scan ""). Reject at substitution; the
	// author must explicitly handle nil via an if-cond (e.g.
	// `if step.x.empty == null` per Design question 4).
	scope := mapScope{"input.maybe": nil}
	_, err := Substitute("{{ input.maybe }}", scope)
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError: %v", err, err)
	}
	if ee.Code != EvalCodeInvalidScalar {
		t.Errorf("err.Code = %q, want %q (AWF4004)", ee.Code, EvalCodeInvalidScalar)
	}
}

func TestSubstituteRejectsNonScalar(t *testing.T) {
	// A ref resolving to a map/slice can't be rendered into a {{ }} slot. Spec
	// §7: "typed scalars rendered to string." Composite values are an
	// AWF4004 InvalidScalar error.
	scope := mapScope{"step.x.obj": map[string]any{"k": "v"}}
	_, err := Substitute("{{ step.x.obj }}", scope)
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err is %T, want *EvalError", err)
	}
	if ee.Code != EvalCodeInvalidScalar {
		t.Errorf("err.Code = %q, want %q (AWF4004)", ee.Code, EvalCodeInvalidScalar)
	}
}

func TestEvalBoolLiterals(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"true && true", true},
		{"true && false", false},
		{"false || true", true},
		{"false || false", false},
		{"!true", false},
		{"!false", true},
		{"!!true", true},
		{"(true || false) && !false", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			got, err := EvalBool(e, mapScope{})
			if err != nil {
				t.Fatalf("EvalBool: %v", err)
			}
			if got != c.want {
				t.Errorf("EvalBool(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestEvalBoolComparisons(t *testing.T) {
	scope := mapScope{
		"input.n":     5.0,
		"input.s":     "abc",
		"input.b":     true,
		"step.x.code": 0,
	}
	cases := []struct {
		src  string
		want bool
	}{
		// numeric (json-unmarshaled numbers are float64; literal NumberLit decodes to float64 too)
		{"input.n == 5", true},
		{"input.n == 6", false},
		{"input.n != 6", true},
		{"input.n < 6", true},
		{"input.n <= 5", true},
		{"input.n > 4", true},
		{"input.n >= 5", true},
		// string
		{`input.s == "abc"`, true},
		{`input.s != "abc"`, false},
		// bool
		{"input.b == true", true},
		{"input.b != false", true},
		// int-typed exit_code compares against numeric literal
		{"step.x.code == 0", true},
		// mixed shape from the Appendix-A gate condition (with composed && / ==)
		{"input.b && input.n == 5", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			got, err := EvalBool(e, scope)
			if err != nil {
				t.Fatalf("EvalBool: %v", err)
			}
			if got != c.want {
				t.Errorf("EvalBool(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestEvalBoolNullComparison(t *testing.T) {
	// Special case per spec §7 Appendix-A: `step.x.empty == null` is a valid
	// check, NOT a type mismatch (Design question 4). Ordered ops on null still
	// error AWF4003.
	scope := mapScope{
		"step.x.empty":     nil,
		"step.x.populated": "value",
	}
	cases := []struct {
		src         string
		want        bool
		errExpected bool
	}{
		{"step.x.empty == null", true, false},
		{"step.x.empty != null", false, false},
		{"step.x.populated == null", false, false},
		{"step.x.populated != null", true, false},
		{"null == null", true, false},
		{"null != null", false, false},
		// Ordered ops on null are still type errors.
		{"step.x.empty < null", false, true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, _ := ParseExpr(c.src)
			got, err := EvalBool(e, scope)
			if c.errExpected {
				var ee *EvalError
				if !errors.As(err, &ee) || ee.Code != EvalCodeTypeMismatch {
					t.Errorf("err = %v, want AWF4003", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EvalBool: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestEvalBoolTypeMismatchIsAWF4003(t *testing.T) {
	scope := mapScope{"input.n": 5.0, "input.s": "abc"}
	cases := []string{
		`input.n == "5"`, // number vs string
		`input.s == 5`,   // string vs number
		`input.n < "5"`,  // ordered compare across types
		`true == 1`,      // bool vs number
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			e, err := ParseExpr(src)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = EvalBool(e, scope)
			var ee *EvalError
			if !errors.As(err, &ee) {
				t.Fatalf("err is %T, want *EvalError: %v", err, err)
			}
			if ee.Code != EvalCodeTypeMismatch {
				t.Errorf("err.Code = %q, want %q (AWF4003)", ee.Code, EvalCodeTypeMismatch)
			}
		})
	}
}

func TestEvalBoolNonBoolTopLevelIsAWF4003(t *testing.T) {
	// EvalBool's top-level value MUST be a bool. A literal-only "5" or "input.n"
	// (where input.n is a number) is a type mismatch.
	scope := mapScope{"input.n": 5.0}
	for _, src := range []string{"5", "input.n", `"hello"`} {
		t.Run(src, func(t *testing.T) {
			e, _ := ParseExpr(src)
			_, err := EvalBool(e, scope)
			var ee *EvalError
			if !errors.As(err, &ee) || ee.Code != EvalCodeTypeMismatch {
				t.Errorf("EvalBool(%q): err = %v, want AWF4003", src, err)
			}
		})
	}
}

func TestEvalBoolShortCircuit(t *testing.T) {
	// && right-side MUST NOT evaluate if left is false; || right-side MUST NOT
	// evaluate if left is true. Detect via a ref that would error if resolved.
	scope := mapScope{"input.b": false, "input.t": true}
	// If && evaluated the right side, missing.ref would AWF4002 — but left is false.
	e, _ := ParseExpr("input.b && missing.ref")
	got, err := EvalBool(e, scope)
	if err != nil {
		t.Errorf("&& short-circuit failed: %v", err)
	}
	if got != false {
		t.Errorf("got %v, want false", got)
	}
	// Same for ||.
	e, _ = ParseExpr("input.t || missing.ref")
	got, err = EvalBool(e, scope)
	if err != nil {
		t.Errorf("|| short-circuit failed: %v", err)
	}
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestEvalBoolRefOversizeIsAWF4001(t *testing.T) {
	// The AWF4001 size check fires at REF RESOLUTION — uniformly in Substitute
	// and EvalBool. A condition comparing a too-large output value to a literal
	// must reject before the comparison runs.
	huge := strings.Repeat("a", MaxInlineBytes+1)
	scope := mapScope{"step.dump.payload": huge}
	e, _ := ParseExpr(`step.dump.payload == "x"`)
	_, err := EvalBool(e, scope)
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Code != EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}
