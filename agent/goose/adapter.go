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
)

// Adapter is the agent.Adapter implementation for Block goose. One Adapter
// per CLI invocation; multiple Launch calls per Adapter; all state read-only
// after construction (no session state crosses Launch boundaries).
type Adapter struct {
	env     agent.SecretEnv   // env-var allowlist (NAME → VALUE) forwarded into each `goose run`
	backend container.Backend // for Version + Launch; nil → those methods error
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

// Capabilities returns Caps{NativeSchema: false} — goose structures output via
// the layer-2 path; the engine validates conformance.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: false}
}
