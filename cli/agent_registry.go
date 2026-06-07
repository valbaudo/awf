package cli

import (
	"fmt"
	"maps"
	"os"
	"sort"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// defaultAgentEnv is the union of every registered adapter's DefaultEnvAllowlist.
// It is the default for `awf run --agent-env` and the implicit allowlist for
// `awf resume`. New adapters extend it by appending their DefaultEnvAllowlist.
var defaultAgentEnv = func() []string {
	out := append([]string{}, claude.DefaultEnvAllowlist...)
	out = append(out, droid.DefaultEnvAllowlist...)
	out = append(out, goose.DefaultEnvAllowlist...)
	out = append(out, codex.DefaultEnvAllowlist...)
	out = append(out, awfllm.DefaultEnvAllowlist...)
	// Dedup: goose's ANTHROPIC_API_KEY overlaps claude's; OPENAI_API_KEY overlaps
	// goose/codex/awfllm — keep first occurrence, preserve order (don't surface a
	// duplicate in the --agent-env default).
	seen := make(map[string]struct{}, len(out))
	deduped := out[:0]
	for _, k := range out {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		deduped = append(deduped, k)
	}
	return deduped
}()

// mergeWorkflowEnv returns the env-var NAME allowlist forwarded into agent
// adapters: the CLI base allowlist (from --agent-env, or the default) plus the
// workflow's own top-level env: declarations (awf-workflow(5)). The workflow
// names extend the base — appended after it, deduped, order-stable (first
// occurrence wins) — so a name declared in both places is forwarded once. The
// values themselves are never read here; only names flow through to
// buildAgentRegistry, which resolves each via os.LookupEnv. The result is always
// a fresh slice (it never aliases base, so a caller can't corrupt the input); an
// all-empty result stays nil to preserve buildAgentRegistry's "no allowlist →
// empty registry" contract.
func mergeWorkflowEnv(base, workflowEnv []string) []string {
	seen := make(map[string]struct{}, len(base)+len(workflowEnv))
	out := make([]string, 0, len(base)+len(workflowEnv))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range base {
		add(name)
	}
	for _, name := range workflowEnv {
		add(name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildAgentRegistry constructs the production *agent.Registry for a CLI
// invocation. Parallels cli/backend.go's newBackend (slice 4.5) — same
// "read flag → build → return" shape, no Runner field needed.
//
// envAllowlist is the list of env-var NAMES to forward into each
// `claude -p` invocation. The function reads each from os.Environ; ones
// missing from the host are silently omitted from the forwarded set
// (per Phase 5 design decision 8 — auth failures surface at Launch time,
// not at build time).
//
// Empty envAllowlist → registry is returned EMPTY (no Claude adapter
// registered). Workflows using `uses: anthropic/claude-code` then fail at
// run start with *agent.ErrAdapterNotFound — clear operator message.
//
// backend is required (the Claude adapter needs it for Version + Launch).
// Tests inject container.NewFake(); production passes the CLI's
// constructed Backend (cli/backend.go resolveBackend result).
func buildAgentRegistry(envAllowlist []string, backend container.Backend) (*agent.Registry, error) {
	if backend == nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: nil backend (Claude adapter needs Backend for Version + Launch)")
	}
	var reg agent.Registry

	if len(envAllowlist) == 0 {
		return &reg, nil
	}

	env := make(map[string]string, len(envAllowlist))
	for _, name := range envAllowlist {
		if name == "" {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}

	adapter, err := claude.New(claude.WithEnv(env), claude.WithBackend(backend))
	if err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: construct claude adapter: %w", err)
	}
	if err := reg.Register(adapter); err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: register claude adapter: %w", err)
	}

	dadapter, err := droid.New(droid.WithEnv(env), droid.WithBackend(backend))
	if err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: construct droid adapter: %w", err)
	}
	if err := reg.Register(dadapter); err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: register droid adapter: %w", err)
	}

	gadapter, err := goose.New(goose.WithEnv(env), goose.WithBackend(backend))
	if err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: construct goose adapter: %w", err)
	}
	if err := reg.Register(gadapter); err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: register goose adapter: %w", err)
	}

	cadapter, err := codex.New(codex.WithEnv(env), codex.WithBackend(backend))
	if err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: construct codex adapter: %w", err)
	}
	if err := reg.Register(cadapter); err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: register codex adapter: %w", err)
	}

	// awf/llm is containerless (direct HTTP) — it takes NO backend; it needs an
	// *http.Client. Production uses the default client: Go's http.DefaultTransport
	// already honors HTTP(S)_PROXY/NO_PROXY (http.ProxyFromEnvironment), so proxying
	// works with no knob; TLS-insecure is a per-step `tls_insecure` with-key the
	// adapter handles internally (clientFor). The adapter's own tests inject a fake
	// RoundTripper via WithHTTPClient. The shared env carries OPENAI_API_KEY.
	lladapter, err := awfllm.New(awfllm.WithEnv(env))
	if err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: construct awf/llm adapter: %w", err)
	}
	if err := reg.Register(lladapter); err != nil {
		return nil, fmt.Errorf("cli: buildAgentRegistry: register awf/llm adapter: %w", err)
	}
	return &reg, nil
}

// registerRoles registers one DerivedAdapter per declared agents: role, AFTER
// the base adapters are in reg. Each role's model/system_prompt fold into the
// role with: as opaque keys (the base adapter reads them); the derived adapter
// then overlays the step's own with: ON TOP of that at dispatch time.
// Registering under the role name makes the role a first-class pinned
// runtime (run.started.Runtimes), so resume drift-checks its resolved base
// version (cli/runtimes.go's resolveRuntimes Lookup is unchanged).
//
// A role whose uses: base is unregistered → *agent.ErrAdapterNotFound; a role
// name colliding with a registered ref → *agent.ErrAdapterAlreadyRegistered
// (defense — AWF1033 already rejects '/' role names statically, but a bare
// collision must still fail loud rather than silently overwrite a base adapter).
func registerRoles(reg *agent.Registry, wf *ir.Workflow) error {
	if wf == nil || len(wf.Agents) == 0 {
		return nil
	}
	// Deterministic order: sort role names so a duplicate/lookup error is stable.
	names := make([]string, 0, len(wf.Agents))
	for n := range wf.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		role := wf.Agents[name]
		base, ok := reg.Lookup(role.Uses)
		if !ok {
			return &agent.ErrAdapterNotFound{Ref: role.Uses}
		}
		if err := reg.Register(agent.NewDerivedAdapter(name, base, roleWithFor(role))); err != nil {
			return fmt.Errorf("cli: register role %q: %w", name, err)
		}
	}
	return nil
}

// roleWithFor folds the role's convenience fields (model, system_prompt) into
// the role with: as opaque keys — the canonical place the engine deposits them
// so the base adapter reads them uniformly with a step's own with:. Never reads
// an existing with: key for its decision (only sets the two convenience keys
// when non-empty and ABSENT, so an explicit role with: value still wins). The
// result is a fresh map (it never aliases role.With).
func roleWithFor(role ir.AgentRole) ir.RawConfig {
	out := make(ir.RawConfig, len(role.With)+2)
	maps.Copy(out, role.With) // key-blind copy; never aliases role.With (out is fresh)
	if role.Model != "" {
		if _, set := out["model"]; !set {
			out["model"] = role.Model
		}
	}
	if role.SystemPrompt != "" {
		if _, set := out["system_prompt"]; !set {
			out["system_prompt"] = role.SystemPrompt
		}
	}
	return out
}
