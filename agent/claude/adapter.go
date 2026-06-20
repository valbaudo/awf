// Package claude implements agent.Adapter against the Claude Code CLI.
// Per Phase 5 design decision 2, the concrete adapter lives in a sub-
// package of agent/ (mirroring container/docker + container/native from
// Phase 4); the agent package itself stays import-free of harness specifics.
//
// Phase 5 design decision 16 — Capabilities().NativeSchema is true: claude's
// --json-schema flag handles spec §4.2 layers 1+3 (constrained output +
// internal retry) inside the CLI. The adapter passes the step's
// OutputSchema straight to --json-schema and reads structured_output from
// the result event. Layer 2 (free-form parsing) is NEVER reached for this
// adapter; future non-native-schema adapters route through conformance
// Bucket 15 with their own structuring-call implementation (Phase 5 design
// Appendix H).
//
// Phase 5 design decision 7 — gate independence is enforced structurally:
//   - Every Launch call is a fresh `claude -p` invocation.
//   - The command line NEVER contains --continue / --resume / --session-id.
//   - --no-session-persistence is ALWAYS passed so the host's
//     ~/.claude/projects/ session journal never records AWF runs.
//   - ValidateConfig rejects with-keys named session_id/continue/resume
//     with *ErrSessionReuseAttempted.
//
// Streaming: Launch parses claude's --output-format stream-json line-by-
// line via io.Pipe + bufio.Scanner from the streaming Backend.Exec chunks
// channel (slice 5.3 Group A refactor). Each line becomes one
// streamMessage; an assistant message may split into multiple AgentEvents
// (one per content block). Launch returns IMMEDIATELY under the γ contract
// with events + outcome channels open; the parser goroutine writes events
// progressively, then sends AgentOutcome on outcomeCh after claude exits.
//
// (Slice 5.3 r2: doc-comment was in doc.go in r1; collapsed here per rule 2.)
package claude

import (
	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// Adapter is the agent.Adapter implementation for Claude Code. Constructed
// by cli/agent_registry.go at CLI start-time and registered in *agent.Registry.
//
// Lifetime: one Adapter per CLI invocation. Multiple Launch calls per
// Adapter (one per AgentStep dispatch; one per gate attempt; one per
// retry). All state held in Adapter is read-only after construction —
// Phase 5 design decision 7 (no session state crosses Launch boundaries).
type Adapter struct {
	env     agent.SecretEnv   // env-var allowlist (NAME → VALUE) forwarded into each `claude -p` exec
	backend container.Backend // for Version + Launch; nil → those methods error
}

// Option configures the Adapter at construction time. Functional-options
// pattern matches the rest of the codebase (clock.IDGen options,
// signal.NewBroker, etc.).
type Option func(*Adapter)

// WithEnv supplies the env-var allowlist the Adapter will forward into
// each `claude -p` invocation's exec environment. Keys are env var names
// (e.g. ANTHROPIC_API_KEY); values are the secrets read from the host
// environment. The map is copied into agent.SecretEnv (which redacts in
// fmt verbs and is json:"-" so secrets cannot reach the state log).
//
// Production wiring: cli/agent_registry.go reads each named env var from
// os.Environ and passes the present ones via WithEnv. Tests inject
// directly. Empty map is permitted (auth will fail at Launch time with
// claude's "Not logged in" if no working credentials).
func WithEnv(env map[string]string) Option {
	return func(a *Adapter) {
		if len(env) == 0 {
			a.env = agent.SecretEnv{}
			return
		}
		out := make(agent.SecretEnv, len(env))
		for k, v := range env {
			out[k] = v
		}
		a.env = out
	}
}

// WithBackend supplies the container.Backend the adapter uses to run
// claude inside the handle (Version, Launch). Required for Version and
// Launch to function; tests that only need Ref/Capabilities/ValidateConfig
// may omit it. Production wiring at cli/agent_registry.go passes the
// CLI's constructed Backend.
//
// (Slightly redundant with Backend.Exec receiving handle on every call,
// but the adapter needs the Backend reference to call Exec at all; the
// Backend's identity is configured once at construction.)
func WithBackend(b container.Backend) Option {
	return func(a *Adapter) {
		a.backend = b
	}
}

// New constructs an Adapter. Returns error only if an Option fails (none
// do today; the error return is for forward-compatibility with options
// that might need to validate, e.g. WithBackend in Task 15).
func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{
		env: agent.SecretEnv{},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Ref returns the agent-runtime identifier this adapter satisfies.
func (*Adapter) Ref() string { return AdapterRef }

// Capabilities returns Caps{NativeSchema: true} — Claude Code's
// --json-schema flag handles spec §4.2 layers 1+3 natively.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: true}
}

// RequiredEnv implements agent.CredentialNamer. Returns the CREDENTIAL env var
// names claude-code authenticates with. All three are defined in
// DefaultEnvAllowlist. Per errors.go / ErrBareRequiresAPIKey, bare mode needs
// ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN; CLAUDE_CODE_OAUTH_TOKEN is the
// OAuth path. At least one being set is sufficient for auth to proceed.
func (*Adapter) RequiredEnv() []string {
	return DefaultEnvAllowlist
}

// envAllowlist returns the adapter's env-passthrough map. Internal — tests
// use it via the unexported method, callers go through Launch where the
// adapter merges into the exec environment.
func (a *Adapter) envAllowlist() agent.SecretEnv {
	return a.env
}
