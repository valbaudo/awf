package cli

import (
	"fmt"
	"os"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
)

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
	return &reg, nil
}
