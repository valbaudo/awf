package cli

import (
	"fmt"
	"os"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
)

// defaultAgentEnv is the union of every registered adapter's DefaultEnvAllowlist.
// It is the default for `awf run --agent-env` and the implicit allowlist for
// `awf resume`. New adapters extend it by appending their DefaultEnvAllowlist.
var defaultAgentEnv = func() []string {
	out := append([]string{}, claude.DefaultEnvAllowlist...)
	out = append(out, droid.DefaultEnvAllowlist...)
	return out
}()

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
	return &reg, nil
}
