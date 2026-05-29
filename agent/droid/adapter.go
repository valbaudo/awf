// Package droid implements agent.Adapter against Factory AI's `droid` CLI
// (the `droid exec` non-interactive subcommand). It mirrors agent/claude; the
// agent package itself stays import-free of harness specifics.
//
// Capabilities().NativeSchema is FALSE: droid has no native JSON-schema flag.
// The adapter runs `droid exec -o json`, parses the result envelope's `result`
// text into a map, and the engine re-validates it against the step's
// output_schema (engine/schema.go ValidateOutputMap). This is the spec §4.2
// layer-2 path (conformance Bucket 15).
//
// Gate independence is enforced structurally: every Launch is a fresh
// `droid exec`; the command line NEVER contains --session-id / --resume /
// --fork; ValidateConfig rejects with-keys session_id/resume/fork/continue.
//
// Output: `droid exec -o json` emits a SINGLE result envelope line on stdout.
// Launch reads it in one goroutine under the γ contract (returns immediately
// with both channels open; drains chunks; emits one event; sends one outcome).
package droid

import (
	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// Adapter is the agent.Adapter implementation for Factory droid. One Adapter
// per CLI invocation; multiple Launch calls per Adapter; all state read-only
// after construction (no session state crosses Launch boundaries).
type Adapter struct {
	env     agent.SecretEnv   // env-var allowlist (NAME → VALUE) forwarded into each `droid exec`
	backend container.Backend // for Version + Launch; nil → those methods error
}

// Option configures the Adapter at construction time (functional-options).
type Option func(*Adapter)

// WithEnv supplies the env-var allowlist forwarded into each `droid exec` exec
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

// New constructs an Adapter.
func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{env: agent.SecretEnv{}}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Ref returns the agent-runtime identifier this adapter satisfies.
func (*Adapter) Ref() string { return AdapterRef }

// Capabilities returns Caps{NativeSchema: false} — droid structures output via
// the layer-2 path; the engine validates conformance.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: false}
}
