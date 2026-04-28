// Package agent provides the Adapter interface — the seam between the
// engine's dispatcher and external agent CLIs (Claude Code first, per
// AgentWorkflowFormat.md §4.2 and runtime-design.md §8).
//
// Phase 5 slice 5.1 ships only the interface, the typed `Registry` value
// type, the read-only `Resolver` subset, the shared types
// (AgentInvocation / AgentResult / AgentEvent / MetricSet / Caps), and
// the in-memory scripted fake (sub-package agent/fake). Slice 5.2 wires
// AgentStep dispatch in engine/local_dispatcher.go to call
// Adapter.Launch via Resolver. Slice 5.3 adds the real `agent/claude`
// sub-package backed by the Claude Code CLI. Cross-impl conformance is
// the conformance.RunSuite + RunAgentSuite job (Buckets 12/13/14/15).
//
// Per runtime-design §3, agent depends on container (for Handle and
// Backend in adapter implementations) and ir (for RawConfig + JSONSchema
// in AgentInvocation). It does NOT depend on engine or state — Adapter
// impls execute work and return typed results; the dispatcher writes to
// state.
//
// Capabilities() reports the adapter's claims about its typed-output
// pipeline. NativeSchema = true means the harness validates against the
// schema internally (Claude Code's --json-schema). NativeSchema = false
// means the adapter must produce typed output some other way (the
// structuring-call pattern — see Phase 5 design Appendix H); Bucket 15
// (conformance.RunSuite) enforces the contract for such adapters.
//
// SECURITY: AgentInvocation.Env is type SecretEnv whose Stringer +
// GoStringer methods redact values in `%v` / `%s` / `%q` / `%#v`
// formatting. **`%+v` BYPASSES Stringer via reflection** and will leak
// the API key. Callers MUST NOT use `%+v` on AgentInvocation in any
// code path that may reach logs, error wrapping, or CI output. Use named
// fields explicitly:
//
//	fmt.Errorf("launch %s in %s failed: %w", inv.Uses, inv.NodePath, err)  // safe
//	fmt.Errorf("launch failed: %+v: %w", inv, err)                          // LEAKS env value
//
// Phase 6 obs's OTel projection MUST NOT attach AgentInvocation.Env to
// any span attribute. The state log (Phase 1.5) is already env-free by
// design.
package agent
