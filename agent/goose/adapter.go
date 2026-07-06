// Package goose implements agent.Adapter against Block's `goose` CLI (the
// `goose run` non-interactive subcommand). It mirrors agent/droid; the agent
// package itself stays import-free of harness specifics.
//
// Capabilities().NativeSchema is FALSE: goose's native typed-output path
// (recipe final_output) is ignored by the claude-code provider. The adapter
// runs `goose run -q --output-format stream-json --no-session`, concatenates the
// assistant text deltas, parses a JSON object out of the reassembled final text
// into a map, and the engine re-validates it against output_schema
// (engine/schema.go ValidateOutputMap). This is the spec §4.2 layer-2 path
// (conformance Bucket 15).
//
// Gate independence is structural: every Launch passes --no-session and never a
// resume/name/session-id/path/fork/interactive flag; ValidateConfig rejects the
// corresponding with-keys.
package goose

import (
	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/pricing"
)

// Adapter is the agent.Adapter implementation for Block goose. One Adapter
// per CLI invocation; multiple Launch calls per Adapter; all state read-only
// after construction (no session state crosses Launch boundaries).
type Adapter struct {
	env     agent.SecretEnv   // env-var allowlist (NAME → VALUE) forwarded into each `goose run`
	backend container.Backend // for Version + Launch; nil → those methods error
	pricer  pricing.Table     // model→rates for the derived USD cost; defaults to pricing.Default()
}

// Option configures the Adapter at construction time (functional-options).
type Option func(*Adapter)

// WithEnv supplies the env-var allowlist forwarded into each `goose run` exec
// environment. Copied into agent.SecretEnv (redacts under fmt verbs; json:"-"
// so secrets never reach the state log). Empty map permitted.
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

// Capabilities returns Caps{NativeSchema: false} — goose structures output via
// the layer-2 path; the engine validates conformance.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: false}
}

// RequiredEnv implements agent.CredentialNamer. Returns the CREDENTIAL env var
// names goose authenticates with (provider-alternatives). The provider itself
// is selected by with:provider or the GOOSE_PROVIDER env var (F34: with:
// provider takes precedence — see resolveProvider); GOOSE_PROVIDER and
// GOOSE_MODEL are config selectors (not credentials) and are intentionally
// excluded here. Both names are defined in DefaultEnvAllowlist.
func (*Adapter) RequiredEnv() []string {
	return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
}
