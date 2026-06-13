package engine

import (
	"encoding/json"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// toolImplScope wraps a base *Scope, intercepting the two react tool-impl roots
// (args_file, args.<field>) and delegating everything else to base. It is NOT an
// edit to Scope.Resolve — keeping args.* out of general workflow scope so the two
// roots exist only inside a tool impl's templated run: (the reduceTemplateScope
// intercept-then-delegate pattern). Spec §3.3.
type toolImplScope struct {
	base     *Scope
	argsFile string         // the per-call container path of the staged verbatim args
	args     map[string]any // parsed args (best-effort); only scalar leaves are served
}

func newToolImplScope(rs *RunState, wf *ir.Workflow, ctxPath, argsFile string, args map[string]any) *toolImplScope {
	return &toolImplScope{base: NewScope(rs, wf, ctxPath), argsFile: argsFile, args: args}
}

// Resolve implements template.Scope. args_file → the staged container path;
// args.<field> → the parsed scalar (absent if non-scalar or unparseable);
// everything else delegates to the base workflow scope.
func (s *toolImplScope) Resolve(ref *template.Ref) (any, error) {
	g := ref.Segments
	if len(g) == 1 && !g[0].IsIndex && g[0].Ident == "args_file" {
		return s.argsFile, nil
	}
	if len(g) == 2 && !g[0].IsIndex && g[0].Ident == "args" && !g[1].IsIndex {
		if v, ok := s.args[g[1].Ident]; ok && isScalar(v) {
			return v, nil
		}
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"args.%s is not a bound scalar (use {{ args_file }})", g[1].Ident)
	}
	return s.base.Resolve(ref)
}

// isScalar reports whether v is a JSON scalar safe to interpolate into a command
// line. JSON numbers arrive as float64 after json.Unmarshal into map[string]any;
// int/int64/json.Number are admitted for Go-constructed args (tests).
func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, int, int64, json.Number:
		return true
	default:
		return false
	}
}
