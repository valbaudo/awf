package ir

// AgentRole is one named, reusable agent configuration in the top-level
// `agents:` map. It binds an EXISTING adapter (Uses) plus opaque defaults the
// base adapter consumes: Model and SystemPrompt are convenience fields that the
// run-start resolver folds into the role's With map as opaque keys (the base
// adapter — e.g. claude — already reads with["model"]/with["system_prompt"]).
// With carries arbitrary opaque base-adapter config (e.g. mcp_servers — the
// memory MCP handle). AWF never interprets a With key; the named adapter
// validates it.
//
// A role does NOT carry a typed-output schema: the typed-output contract is the
// STEP's own output_schema (engine/agent_step.go sources inv.OutputSchema from
// the AgentStep, never from with:), and the with: overlay seam cannot reach it
// (matching design spec §3.3 — a role is adapter + model + system_prompt + with:
// only).
type AgentRole struct {
	Uses         string    `json:"uses"`
	Model        string    `json:"model,omitempty"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	With         RawConfig `json:"with,omitempty"`
}

// RoleByName returns the declared role (ok=false if none). The single accessor
// the validator and the run-start resolver share so role lookup lives in one
// place.
func (w *Workflow) RoleByName(name string) (AgentRole, bool) {
	r, ok := w.Agents[name]
	return r, ok
}
