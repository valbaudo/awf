package template_test

import (
	"testing"

	"github.com/valbaudo/awf/template"
)

func TestUnwrapEnvelope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"plain envelope", "{{ input.x }}", "input.x"},
		{"no_whitespace", "{{input.x}}", "input.x"},
		{"surrounding_whitespace", "  {{ input.x }}  ", "input.x"},
		{"missing_envelope", "input.x", "input.x"},
		{"empty_string", "", ""},
		{"only_open", "{{ input.x", "{{ input.x"},
		{"only_close", "input.x }}", "input.x }}"},
		{"complex_expression", "{{ step.a.value == 5 && input.b }}", "step.a.value == 5 && input.b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := template.UnwrapEnvelope(c.in); got != c.want {
				t.Errorf("UnwrapEnvelope(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
