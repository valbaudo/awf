package template

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEvalTemplateValuePreservesWholeRefObject(t *testing.T) {
	want := map[string]any{
		"items": []any{"a", "b"},
		"meta":  map[string]any{"count": 2.0},
	}
	raw := json.RawMessage(`"{{ step.seed.payload }}"`)

	got, err := EvalTemplateValue(raw, mapScope{"step.seed.payload": want})
	if err != nil {
		t.Fatalf("EvalTemplateValue: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestEvalTemplateValuePreservesArray(t *testing.T) {
	raw := json.RawMessage(`["{{ input.items }}","{{ run.id }}",3,true,null]`)
	want := []any{[]any{"x", "y"}, "run-1", 3.0, true, nil}

	got, err := EvalTemplateValue(raw, mapScope{
		"input.items": []any{"x", "y"},
		"run.id":      "run-1",
	})
	if err != nil {
		t.Fatalf("EvalTemplateValue: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestEvalTemplateValueSubstitutesStringWithInlineRef(t *testing.T) {
	raw := json.RawMessage(`"job-{{ run.id }}-{{ step.seed.count }}"`)

	got, err := EvalTemplateValue(raw, mapScope{
		"run.id":          "run-1",
		"step.seed.count": 3,
	})
	if err != nil {
		t.Fatalf("EvalTemplateValue: %v", err)
	}
	if got != "job-run-1-3" {
		t.Errorf("got %#v, want %q", got, "job-run-1-3")
	}
}

func TestEvalTemplateValueRecursesIntoObject(t *testing.T) {
	raw := json.RawMessage(`{"items":"{{ step.seed.items }}","label":"for {{ run.id }}","keep":false}`)
	want := map[string]any{
		"items": []any{"one", "two"},
		"label": "for run-1",
		"keep":  false,
	}

	got, err := EvalTemplateValue(raw, mapScope{
		"step.seed.items": []any{"one", "two"},
		"run.id":          "run-1",
	})
	if err != nil {
		t.Fatalf("EvalTemplateValue: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestEvalTemplateValueRejectsOversize(t *testing.T) {
	raw := json.RawMessage(`"{{ input.payload }}"`)
	huge := strings.Repeat("a", MaxInlineBytes+1)

	_, err := EvalTemplateValue(raw, mapScope{"input.payload": huge})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Code != EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}

func TestEvalTemplateValueRejectsOversizeWholeRefObject(t *testing.T) {
	raw := json.RawMessage(`"{{ input.payload }}"`)
	huge := map[string]any{"blob": strings.Repeat("a", MaxInlineBytes+1)}

	_, err := EvalTemplateValue(raw, mapScope{"input.payload": huge})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Code != EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}

func TestEvalTemplateValueRejectsOversizeWholeRefArray(t *testing.T) {
	raw := json.RawMessage(`"{{ input.payload }}"`)
	huge := []any{strings.Repeat("a", MaxInlineBytes+1)}

	_, err := EvalTemplateValue(raw, mapScope{"input.payload": huge})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Code != EvalCodeOversize {
		t.Errorf("err = %v, want AWF4001", err)
	}
}
