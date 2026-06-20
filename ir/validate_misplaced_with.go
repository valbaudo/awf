package ir

// reservedAgentStepKeys are the json field names of AgentStep (§4 step-level keys)
// EXCLUDING "with" itself. Any key from this set found inside an AgentStep's with:
// block will be silently ignored by the engine — the validator surfaces that as AWF1061.
var reservedAgentStepKeys = map[string]bool{
	"id":              true,
	"container":       true,
	"uses":            true,
	"continues":       true,
	"output_schema":   true,
	"output_files":    true,
	"skills":          true,
	"input_files":     true,
	"timeout":         true,
	"idempotency_key": true,
	"retry":           true,
}

// validateMisplacedWithKeys warns (AWF1061) when a reserved step-level key name
// appears inside an AgentStep's with: block, where the engine never reads it.
// Authors commonly mis-nest input_files or output_schema under with: thinking
// they will be picked up — they won't.
//
//   - AWF1061: a reserved step-level key nested inside with:
func validateMisplacedWithKeys(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	WalkNodes(ld.Workflow.Graph, "", func(n Node, path string) {
		as, ok := n.(*AgentStep)
		if !ok {
			return
		}
		for key := range as.With {
			if reservedAgentStepKeys[key] {
				c.warnf(path, "AWF1061", catalog["AWF1061"])
			}
		}
	})
}
