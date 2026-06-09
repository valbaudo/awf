package template

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EvalTemplateValue decodes raw JSON, then evaluates template slots inside JSON
// string leaves. A string that is exactly one {{ ref }} slot resolves to the
// typed reference value; mixed strings use normal scalar substitution.
func EvalTemplateValue(raw json.RawMessage, scope Scope) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode template value JSON: %w", err)
	}
	return evalTemplateDecodedValue(v, scope)
}

func evalTemplateDecodedValue(v any, scope Scope) (any, error) {
	switch x := v.(type) {
	case string:
		return evalTemplateStringValue(x, scope)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			v, err := evalTemplateDecodedValue(x[i], scope)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, raw := range x {
			v, err := evalTemplateDecodedValue(raw, scope)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	default:
		return v, nil
	}
}

func evalTemplateStringValue(s string, scope Scope) (any, error) {
	slots, err := Slots(s)
	if err != nil {
		var se *SyntaxError
		if errors.As(err, &se) {
			return nil, EvalErrf(EvalCodeSyntax, "slot scan at offset %d: %s", se.Pos, se.Msg)
		}
		return nil, &EvalError{Code: EvalCodeSyntax, Msg: "slot scan: " + err.Error()}
	}
	if len(slots) == 1 && slots[0].Start == 0 && slots[0].End == len(s) {
		ref, err := ParseRef(slots[0].Inner)
		if err != nil {
			var se *SyntaxError
			if errors.As(err, &se) {
				hostPos := slots[0].Start + len(slotOpen) + se.Pos
				return nil, EvalErrf(EvalCodeSyntax, "parse slot at host offset %d: %s", hostPos, se.Msg)
			}
			return nil, &EvalError{Code: EvalCodeSyntax, Msg: "parse slot: " + err.Error()}
		}
		return resolveRefValue(scope, ref)
	}
	return Substitute(s, scope)
}
