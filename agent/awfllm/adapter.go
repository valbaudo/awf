// Package awfllm implements agent.Adapter as a single, streaming LLM call against
// any OpenAI-compatible Chat Completions endpoint (OpenAI, Ollama, vLLM,
// llama.cpp, LM Studio, LiteLLM/Bifrost gateways). Unlike the CLI adapters
// (claude/droid/goose/codex) it is NOT a black-box CLI in a container: Launch
// ignores the container.Handle and issues one HTTP request via an injected
// *http.Client, streaming deltas live (one AgentEvent per chunk) and reassembling
// the full text for a layer-2 parse the engine re-validates.
//
// Capabilities: NativeSchema:false (the adapter parses the model's message
// content itself — strict response_format is a first-try optimization, not the
// contract; the engine's ValidateOutputMap is the contract) and Containerless:true
// (no container needed — see Part A).
package awfllm

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/pricing"
)

// Adapter is the agent.Adapter for a single streaming LLM call. One Adapter per
// CLI invocation; multiple Launch calls per Adapter; read-only after construction.
type Adapter struct {
	env        agent.SecretEnv // env-var allowlist (NAME → VALUE); the API key rides here
	httpClient *http.Client    // injected for determinism + fake-transport tests
	pricer     pricing.Table   // model→rates for derived USD cost; defaults to pricing.Default()

	geminiCacheMu  sync.RWMutex      // guards geminiCacheMap (concurrent Launch in fan-out)
	geminiCacheMap map[string]string // contentKey → server-assigned CachedContent name
}

// Option configures the Adapter (functional options).
type Option func(*Adapter)

// WithEnv supplies the env allowlist (the API key is read from here by name).
// Copied into agent.SecretEnv (redacts under fmt verbs; json:"-"). Empty OK.
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

// WithHTTPClient injects the *http.Client used for every call. Tests inject a
// fake RoundTripper (no network). If unset, New installs a default client whose
// timeout is generous so it never preempts the per-call ctx deadline.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) { a.httpClient = c }
}

// WithPricing injects the pricing.Table used to derive a USD cost from token
// usage. Tests pass a self-contained fixture table; production leaves it unset so
// New defaults it to pricing.Default() (embedded rates + $AWF_PRICING_FILE).
func WithPricing(t pricing.Table) Option {
	return func(a *Adapter) { a.pricer = t }
}

// New constructs an Adapter.
func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{env: agent.SecretEnv{}}
	for _, opt := range opts {
		opt(a)
	}
	if a.httpClient == nil {
		// Generous timeout: ctx (carrying the step `timeout:`) is the real
		// deadline authority; this is only a backstop against a hung socket.
		a.httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	if a.pricer == nil {
		a.pricer = pricing.Default()
	}
	a.geminiCacheMap = make(map[string]string)
	return a, nil
}

// Ref returns the agent-runtime identifier this adapter satisfies.
func (*Adapter) Ref() string { return AdapterRef }

// Capabilities: layer-2 typed output (NativeSchema:false) + no container needed +
// threading supported (engine-supplied continues: message history prepended by launch).
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{NativeSchema: false, Containerless: true, Threaded: true, ContextEvidence: true}
}

// RunToolLoop executes ONE model call with tools attached (P3 A3 — the engine-
// mediated tool loop). It validates the react with: (prompt-exempt), builds the
// reqConfig via the shared buildReqConfig, rejects the Ollama-native path (no
// tool-call support in v1), and delegates the call to runOneToolCall. The engine
// (runReact) owns the message history and the round loop; this method is one round.
func (a *Adapter) RunToolLoop(ctx context.Context, inv agent.ToolLoopInvocation) (agent.ToolLoopResult, error) {
	if err := a.validateConfigForToolLoop(inv.With); err != nil {
		return agent.ToolLoopResult{}, err
	}
	// buildReqConfig takes an AgentInvocation (config.go); it reads only With + a.env.
	// Pass a throwaway invocation carrying our With (no prompt key — react owns turns).
	cfg, err := a.buildReqConfig(agent.AgentInvocation{With: inv.With})
	if err != nil {
		return agent.ToolLoopResult{}, err
	}
	if cfg.Provider == providerOllama {
		return agent.ToolLoopResult{}, fmt.Errorf("agent/awfllm: react: not supported on the Ollama-native transport (provider: ollama / structured_output: ollama_format)")
	}
	if cfg.Provider == providerAnthropic {
		return agent.ToolLoopResult{}, fmt.Errorf("agent/awfllm: react: not supported on provider: anthropic in v1 (native tool-loop is a future design)")
	}
	return a.runOneToolCall(ctx, cfg, inv.NodePath, inv.Messages, inv.Tools, inv.OutputSchema)
}

// compile-time interface assertions: Adapter satisfies the full agent.Adapter
// contract and the optional ToolLoopRunner (react:) contract.
var (
	_ agent.Adapter        = (*Adapter)(nil)
	_ agent.ToolLoopRunner = (*Adapter)(nil)
)
