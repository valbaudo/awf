// Package codex implements agent.Adapter against OpenAI's `codex` CLI (the
// `codex exec` non-interactive subcommand). It mirrors agent/goose for the
// Launch/stream scaffold and agent/claude for the NativeSchema:true contract;
// the agent package itself stays import-free of harness specifics.
//
// Capabilities().NativeSchema is TRUE: `codex exec --output-schema <FILE>`
// API-constrains the model's final response (OpenAI Responses structured
// output), so the agent_message text is pure conforming JSON. The adapter writes
// the step's output_schema to a container temp file (via the same sh -c command),
// passes --output-schema, and strict-json.Unmarshals the LAST agent_message text
// into a map; the engine re-validates it against output_schema (engine
// ValidateOutputMap). Layer 2 (free-text parsing) is never reached — codex is
// the first non-Claude NativeSchema:true adapter (conformance Bucket 14).
//
// Gate independence is structural: every Launch runs a bare `codex exec` with
// --ephemeral and never a resume/fork subcommand; ValidateConfig rejects the
// corresponding with-keys.
package codex

import (
	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/pricing"
)

// Adapter is the agent.Adapter implementation for OpenAI codex. One Adapter per
// CLI invocation; multiple Launch calls per Adapter; all state read-only after
// construction (no session state crosses Launch boundaries).
type Adapter struct {
	env     agent.SecretEnv   // env-var allowlist (NAME → VALUE) forwarded into each `codex exec`
	backend container.Backend // for Version + Launch; nil → those methods error
	pricer  pricing.Table     // model→rates for the derived USD cost; defaults to pricing.Default()
}

// Option configures the Adapter at construction time (functional-options).
type Option func(*Adapter)

// WithEnv supplies the env-var allowlist forwarded into each `codex exec` exec
// environment. Copied into agent.SecretEnv (redacts under fmt verbs; json:"-" so
// secrets never reach the state log). Empty map permitted.
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

// WithBackend supplies the container.Backend used by Version and Launch.
func WithBackend(b container.Backend) Option {
	return func(a *Adapter) { a.backend = b }
}

// WithPricing injects the pricing.Table used to derive a USD cost from token
// usage. Tests pass a self-contained fixture table; production leaves it unset so
// New defaults it to pricing.Default() (embedded rates ⊕ $AWF_PRICING_FILE).
func WithPricing(t pricing.Table) Option {
	return func(a *Adapter) { a.pricer = t }
}

// New constructs an Adapter.
func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{env: agent.SecretEnv{}}
	for _, opt := range opts {
		opt(a)
	}
	if a.pricer == nil {
		a.pricer = pricing.Default()
	}
	return a, nil
}

// Ref returns the agent-runtime identifier this adapter satisfies.
func (*Adapter) Ref() string { return AdapterRef }

// Capabilities returns Caps{NativeSchema: true} — codex's --output-schema
// constrains generation API-side; layer 2 is never reached.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: true}
}

// RequiredEnv implements agent.CredentialNamer. Returns the CREDENTIAL env var
// name codex authenticates with. OPENAI_API_KEY is defined in DefaultEnvAllowlist;
// CODEX_HOME is a config directory (not a credential) and is intentionally excluded.
func (*Adapter) RequiredEnv() []string {
	return []string{"OPENAI_API_KEY"}
}
