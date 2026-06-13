# P3 Tool-Loop Keystone (`tools:` + `react:`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a native augmented-LLM agent loop to AWF — a top-level `tools:` block (A4) and a `react:` step (A3) on the `awf/llm` path — so authors can *build* a model+tools+loop agent in config, with exact resume.

**Architecture:** `react:` is a new control-style node (Map-class: a `react[N]` runtime path + an `id` for output addressing). Each round journals two-level: a synthetic `.model` leaf (`node.completed`, reduce-style) carrying the assistant turn + verbatim `tool_calls`, one `.tool-J` leaf per dispatched tool (an ordinary code-step commit), and a thin `react.round` cursor marker (`loop.iter`-shaped payload, gate-style `Sync`). Resume folds completed rounds via `len(ReactRounds[R])+1` and per-leaf `LookupCompleted` guards, so the non-deterministic model is never re-sampled and committed tools never re-run. The model call reaches `awf/llm` through a new optional interface `agent.ToolLoopRunner` (the `ResumePreflighter` pattern), leaving the shared `agent.Adapter` seam untouched.

**Tech Stack:** Go 1.26; `openai-go v3.39.0` (pinned); the existing `engine`/`ir`/`agent`/`container`/`state` packages and their fake-backed conformance harness.

**Source of truth:** the design spec `docs/superpowers/specs/2026-06-13-awf-p3-toolloop-design.md` (read it first). Section refs below (§N) point there.

---

## Conventions for every task

- **Worktree/branch:** work in `.claude/worktrees/p3-toolloop` on branch `feat/p3-toolloop` (already created off `origin/main`). All paths below are relative to that worktree root.
- **Two test commands — both matter:**
  - **Unit tests** (engine/ir/agent) run under `make test` (= `go test -race ./...`). Use the package-scoped form while iterating, e.g. `go test -race ./engine/ -run TestReact...`.
  - **Conformance** lives in `conformance/` and is **build-tag `integ`-gated** — it does **not** run under `make test`. Run it with `go test -tags=integ -count=1 -p 1 ./conformance/ -run TestConformanceFakeBackend/react`.
  - The merge bar is **`make lint test`** green **plus** `go test -tags=integ ./conformance/...` green. `make lint` runs `gofmt -l`, `go vet`, and `golangci-lint` — keep all three clean (per the project's lint discipline: `go vet` alone is insufficient).
- **Commit cadence:** one commit per task (the final step of each task). End every commit message with the `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer.
- **TDD:** write the failing test, run it, watch it fail for the *expected* reason, implement minimally, watch it pass, commit. Never skip the "watch it fail" step.

## File structure (what gets created/modified)

| File | Responsibility | Phase |
|---|---|---|
| `man/awf-workflow.5.md` | Format contract: `tools:` block + `react:` step | 0 |
| `ir/tool.go` (new) | `ir.Tool` + id-less `ir.ToolImpl` types | 1 |
| `ir/node.go` | `ir.React` node type + `isNode()` + doc-count | 1 |
| `ir/node_marshal.go` / `ir/node_unmarshal.go` | React (un)marshal registry edits | 1 |
| `ir/node_test.go` | `wantKinds` 12→13 | 1 |
| `ir/types.go` | `Workflow.Tools` field | 1 |
| `ir/validate_tools.go` (new) | tools/react cross-refs, gating, reserved fields, producer | 1 |
| `ir/validate_refs.go` | `args`/`args_file`/`stop_reason` walkRefs arms + react producer | 1 |
| `engine/path.go` | `roundSep` + `RoundPath`/`ModelPath`/`ToolPath` | 2 |
| `engine/events.go` | `EventReactRound` + `ReactRoundData` | 2 |
| `engine/runstate.go` | `ReactRounds` + `ReactRoundRecord` + accessors | 2 |
| `engine/fold.go` | `EventReactRound` fold arm + pre-alloc | 2 |
| `agent/adapter.go` | `ToolLoopRunner` optional interface + `ToolLoop*` types | 3 |
| `agent/derived.go` | `DerivedAdapter.RunToolLoop` forwarding | 3 |
| `agent/awfllm/adapter.go` | `RunToolLoop` impl + compile-time assertion | 3 |
| `agent/awfllm/transport.go` | tool attach + `tool_calls` parse (OpenAI path) | 3 |
| `agent/awfllm/validate.go` | prompt-exempt `ValidateConfig` variant | 3 |
| `engine/react.go` (new) | `runReact` — the loop, journaling, scope, terminal | 4 |
| `engine/react_scope.go` (new) | `toolImplScope` wrapper | 4 |
| `engine/interpreter.go` | `interpNode` `*ir.React` dispatch arm | 4 |
| `agent/fake/fake.go` | fake `ToolLoopRunner` (scripted tool_calls) | 5 |
| `conformance/react.go` (new) | `testReact` bucket | 5 |
| `conformance/suite.go` | register `react` bucket | 5 |
| `man/awf.1.md` + `README.md` | P2 native-resume doc note | 6 |

---

# Phase 0 — Man-page format revision (the contract, first)

> Per AGENTS.md "the man page is the contract": the format lands in `man/awf-workflow.5.md` before any code. No `go test` here; the verification is that the doc describes exactly what Phases 1–4 implement. If you have the `updating-the-manual` skill, use it.

### Task 0.1: Document the top-level `tools:` block

**Files:**
- Modify: `man/awf-workflow.5.md` (add a `## tools` section near the `## containers` / top-level-keys area)

- [ ] **Step 1: Add the `tools:` section.** Insert this block (adapt heading depth to the surrounding document):

````markdown
## tools

`tools:` is a top-level map (a sibling of `graph:` and `outputs:`) from tool name to a tool
definition. Tools are offered to a `react:` step's model; the model calls them by name.

```yaml
tools:
  <tool-name>:
    description: <string>          # required — sent to the model
    input_schema: <JSON Schema>    # required — the tool's parameters (the §7 floor applies)
    impl:                          # required — how the tool runs
      run: <command>               # or `exec: [argv...]`
      container: <name>            # a containers:-declared name (NOT an inline image)
      timeout: <duration>          # optional
      output_files: { ... }        # optional
      input_files: { ... }         # optional
      retry: { ... }               # optional
```

The model's call arguments reach `impl` two ways: the full arguments JSON is staged into the
container and exposed as `{{ args_file }}`; top-level scalar fields are also bound as
`{{ args.<field> }}` (best-effort — absent if non-scalar or unparseable). Read structured arguments
from `{{ args_file }}`; never interpolate raw arguments into a shell command line.

Each `impl` runs as an ordinary containerful step on the existing execution substrate. The
container is a `containers:`-declared name, digest-pinned there like any step's image.
````

- [ ] **Step 2: Verify.** Re-read the inserted section against spec §3.1 and §3.3 — confirm: declared `container:` (no inline `image:`), `{{ args_file }}` + `{{ args.<field> }}` binding, "read structured args from the file".

- [ ] **Step 3: Commit.**

```bash
git add man/awf-workflow.5.md
git commit -m "docs(man): document the top-level tools: block (P3 A4)"
```

### Task 0.2: Document the `react:` step

**Files:**
- Modify: `man/awf-workflow.5.md` (add a `## react` section in the node-kinds area, after `## gate` / `## map`)

- [ ] **Step 1: Add the `react:` section.** Insert:

````markdown
## react

`react:` is a control node that runs a model + tools loop on the `awf/llm` path. It is the only
node that drives an engine-mediated tool loop; CLI agents (claude/codex/droid/goose) stay
black-box and cannot use it.

```yaml
- react:
    id: <node-id>               # required — addresses the node's output ({{ <id>.* }} / awf outputs --step <id>)
    with:                       # the awf/llm config, minus `prompt`
      uses: awf/llm
      model: <model>
      base_url: <url>
      system_prompt: <string>
    prompt: <template>          # required — the initial user turn
    tools: [<name>, ...]        # required, >=1 — subset of top-level tools: this step offers
    max_turns: <int>            # optional, default 8 — one turn = one model call (+ its tools)
    output_schema: <JSON Schema># optional — the typed final answer (enforced on natural stop only)
```

Each turn: the model is called with `tools` attached; if it requests tools, each is dispatched as
its `impl` step and the results are fed back; the loop repeats until the model stops or `max_turns`
is reached.

**Output contract.** The node's output always carries a reserved top-level `stop_reason` sibling
(`"stop"` | `"max_turns"`). Reference it as `{{ <id>.stop_reason }}` and the answer fields as
`{{ <id>.<field> }}`. On natural stop (`stop_reason: "stop"`) the answer is validated against
`output_schema` if declared. On `max_turns` the loop stops without dispatching the final round's
tools, `output_schema` is **not** enforced, and the output is `{ stop_reason: "max_turns", text: <last assistant text> }`. `output_schema` may not declare a property named `stop_reason`.

**Tool failures the model sees** (not step failures): a tool's non-zero exit feeds its exit code +
stdout back as the tool result; an unknown/hallucinated tool name feeds back an error. The
model-facing tool result is capped (large output is truncated, the full output kept in the run's
artifacts); non-UTF-8 output is referenced, not inlined.

**Scope.** `react:` requires an `awf/llm` (containerless, threaded) adapter; v1 is OpenAI-compat
only (a `structured_output: ollama_format` config is rejected). A top-level `react:` is referenceable
via `{{ <id>.* }}`; a `react:` nested in `loop`/`gate`/`map` is readable via `awf outputs --step`
only.
````

- [ ] **Step 2: Verify** against spec §3.2, §4.5, §5, §6.1 — confirm the output contract, the failure semantics, and the nesting/Ollama scope match exactly.

- [ ] **Step 3: Commit.**

```bash
git add man/awf-workflow.5.md
git commit -m "docs(man): document the react: step (P3 A3)"
```

---

# Phase 1 — A4: `tools:` block IR + validation + digest

### Task 1.1: Add the `ir.Tool` / `ir.ToolImpl` types

**Files:**
- Create: `ir/tool.go`
- Test: `ir/tool_test.go`

- [ ] **Step 1: Write the failing test.**

```go
package ir

import (
	"encoding/json"
	"testing"
)

func TestToolImplOmitsEmptyFields(t *testing.T) {
	// An id-less impl with only run+container must NOT serialize an empty "id"
	// (the reason it is not a reused CodeStep) — and must omit empty optionals.
	impl := ToolImpl{Run: "true", Container: "lab"}
	b, err := json.Marshal(impl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"run":"true","container":"lab"}`
	if got != want {
		t.Fatalf("ToolImpl JSON = %s, want %s", got, want)
	}
}

func TestToolRoundTrip(t *testing.T) {
	in := Tool{
		Description: "Validate an IBAN",
		InputSchema: &JSONSchema{"type": "object"},
		Impl:        ToolImpl{Run: "./validate --args-file {{ args_file }}", Container: "fin"},
	}
	b, _ := json.Marshal(in)
	var out Tool
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Description != in.Description || out.Impl.Run != in.Impl.Run || out.Impl.Container != "fin" {
		t.Fatalf("round-trip lost data: %+v", out)
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`undefined: ToolImpl` / `Tool`).

Run: `go test -race ./ir/ -run 'TestTool'`
Expected: FAIL (compile error: undefined Tool/ToolImpl)

- [ ] **Step 3: Implement `ir/tool.go`.** (Modeled on the id-less `ir.Reduce`, `ir/reduce.go:16-23`.)

```go
package ir

// Tool is one entry of the top-level tools: map (P3 A4). The map KEY is the tool
// name (sent to the model); Tool is the value. A tool is offered to a react: step's
// model, which calls it by name.
type Tool struct {
	Description string      `json:"description"`
	InputSchema *JSONSchema `json:"input_schema"`
	Impl        ToolImpl    `json:"impl"`
}

// ToolImpl is the executable body of a tool — a run:/exec step that names a
// containers:-declared container. It is a DEDICATED id-less type, NOT a reused
// CodeStep: CodeStep.ID is `json:"id"` WITHOUT omitempty, so embedding a CodeStep
// would serialize an empty "id":"" into the JCS workflow digest. At execution time
// the engine synthesizes a real CodeStep from these fields (the reduce.go pattern).
// All fields are omitempty so an absent field never enters the digest.
type ToolImpl struct {
	Run          string            `json:"run,omitempty"`
	Exec         []string          `json:"exec,omitempty"`
	Container    string            `json:"container,omitempty"`
	Timeout      *Duration         `json:"timeout,omitempty"`
	OutputSchema *JSONSchema       `json:"output_schema,omitempty"`
	OutputFiles  OutputFiles       `json:"output_files,omitempty"`
	InputFiles   map[string]string `json:"input_files,omitempty"`
	Retry        *RetryPolicy      `json:"retry,omitempty"`
}
```

> Note: confirm `JSONSchema` is the map type used elsewhere (e.g. `map[string]any` alias) and that `OutputFiles`/`RetryPolicy`/`Duration` are the exact existing type names in `ir/`. If `exec:` support is not present on `CodeStep` today, drop the `Exec` field and document `run:`-only in Phase 0 (keep the plan and man-page consistent).

- [ ] **Step 4: Run it — expect PASS.**

Run: `go test -race ./ir/ -run 'TestTool'`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add ir/tool.go ir/tool_test.go
git commit -m "feat(ir): add Tool + id-less ToolImpl types (P3 A4)"
```

### Task 1.2: Add `Workflow.Tools` and confirm it folds into the digest

**Files:**
- Modify: `ir/types.go` (add the field to `Workflow`)
- Test: `ir/tool_test.go` (append)

- [ ] **Step 1: Write the failing test** (append to `ir/tool_test.go`). The digest is a whole-workflow JCS hash, so adding `tools:` must change the digest — proving it is pinned.

```go
func TestWorkflowToolsAffectDigest(t *testing.T) {
	base := &Workflow{ID: "wf", Version: 1, Graph: NodeList{}}
	withTool := &Workflow{ID: "wf", Version: 1, Graph: NodeList{},
		Tools: map[string]Tool{"t": {Description: "d", InputSchema: &JSONSchema{"type": "object"}, Impl: ToolImpl{Run: "true", Container: "c"}}}}
	d1, err := ComputeDigest(base)
	if err != nil {
		t.Fatalf("digest base: %v", err)
	}
	d2, err := ComputeDigest(withTool)
	if err != nil {
		t.Fatalf("digest withTool: %v", err)
	}
	if d1 == d2 {
		t.Fatal("tools: did not change the workflow digest — not pinned")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`unknown field Tools in struct literal`).

Run: `go test -race ./ir/ -run TestWorkflowToolsAffectDigest`
Expected: FAIL (compile error)

- [ ] **Step 3: Add the field to `Workflow`** in `ir/types.go` (alongside `Graph`/`Outputs`):

```go
	// Tools is the top-level tools: map (P3 A4) — tool name → definition. Offered
	// to react: steps. Folds into the digest automatically (whole-workflow JCS).
	Tools map[string]Tool `json:"tools,omitempty"`
```

- [ ] **Step 4: Run it — expect PASS.** Also run the full `ir` suite to confirm no digest fixtures broke for tool-less workflows (`omitempty` keeps them byte-identical).

Run: `go test -race ./ir/`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add ir/types.go ir/tool_test.go
git commit -m "feat(ir): add Workflow.Tools; pinned via JCS digest (P3 A4)"
```

### Task 1.3: Register the `react:` node kind (the 4 synchronized edits)

**Files:**
- Modify: `ir/node.go` (type + `isNode()` + the 12→13 doc count)
- Modify: `ir/node_marshal.go` (the `wrap` arm)
- Modify: `ir/node_unmarshal.go` (`controlKeys` entry + `unmarshalControl` case)
- Modify: `ir/node_test.go` (`wantKinds` 12→13)
- Test: `ir/node_test.go` (the exhaustive registry test is the failing test)

- [ ] **Step 1: Write the failing test** — add `react:` to `controlKeys` *first* so `TestNodeRegistryExhaustive` fails on the count, proving the forcing function. Actually, do it TDD-style: add the type + the `controlKeys` entry, then run the existing `TestNodeRegistryExhaustive` and watch it fail on `wantKinds`.

  First add the `React` struct + `isNode()` to `ir/node.go` (after the `Map` block):

```go
// React is the model+tools+loop node (P3 A3). A control-style wrapper of the Map
// class: its runtime path is react[N] (keyword[index], NOT its id); the id is for
// output addressing only ({{ <id>.* }} / awf outputs --step <id>) via producer
// registration. tools is required and non-empty; with is the awf/llm config minus
// prompt (the engine owns the messages array). See spec §3.2.
type React struct {
	ID           string      `json:"id,omitempty"`
	With         RawConfig   `json:"with,omitempty"`
	Prompt       string      `json:"prompt"`
	Tools        []string    `json:"tools"`
	MaxTurns     int         `json:"max_turns,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
}

func (*React) isNode() {}
```

- [ ] **Step 2: Add the marshal arm** in `ir/node_marshal.go` (alongside the five `wrap` lines):

```go
func (n *React) MarshalJSON() ([]byte, error) { type a React; return wrap("react", (*a)(n)) }
```

- [ ] **Step 3: Add the unmarshal registry** in `ir/node_unmarshal.go` — the `controlKeys` factory entry and the `unmarshalControl` case (mirror `Map` exactly):

```go
// in controlKeys:
	"react": func() Node { return &React{} },

// in unmarshalControl's switch:
	case *React:
		return json.Unmarshal(raw, n)
```

> Match the existing `unmarshalControl` body for a wrapper-with-id node — copy the `*Map` case's exact form.

- [ ] **Step 4: Run `TestNodeRegistryExhaustive` — expect FAIL** on the count (`registries cover 13 kinds, want 12`).

Run: `go test -race ./ir/ -run TestNodeRegistryExhaustive`
Expected: FAIL ("registries cover 13 kinds, want 12")

- [ ] **Step 5: Bump `wantKinds` 12→13** in `ir/node_test.go` and update the comment (`4 step + 9 control`); update the `ir/node.go:3-16` doc comment count "twelve node kinds (4 step + 8 control)" → "thirteen node kinds (4 step + 9 control)".

- [ ] **Step 6: Run it — expect PASS.** Add a quick round-trip test for the React node and run the full `ir` suite (the tag-reflection test guards the json tags on the new struct):

```go
func TestReactNodeRoundTrip(t *testing.T) {
	in := NodeList{&React{ID: "answer", Prompt: "{{ input.q }}", Tools: []string{"t"}, MaxTurns: 4}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out NodeList
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r, ok := out[0].(*React)
	if !ok || r.ID != "answer" || len(r.Tools) != 1 || r.MaxTurns != 4 {
		t.Fatalf("react round-trip lost data: %#v", out[0])
	}
}
```

Run: `go test -race ./ir/`
Expected: PASS

- [ ] **Step 7: Commit.**

```bash
git add ir/node.go ir/node_marshal.go ir/node_unmarshal.go ir/node_test.go
git commit -m "feat(ir): register react: node kind (4-edit registry, wantKinds 13) (P3 A3)"
```

### Task 1.4: Validate `tools:` + `react:` cross-references and gating

**Files:**
- Create: `ir/validate_tools.go`
- Modify: `ir/validate.go` (call the new pass)
- Test: `ir/validate_tools_test.go`

- [ ] **Step 1: Write the failing tests** (follow the `ir/validate_prune_test.go` pattern: `makeLD` + `assertErrorAt(diags, CODE, "react[0]")`; control path is `react[0]`, NOT the id). Pick unused AWF codes — check `ir/` for the next free numbers; this plan uses `AWF1050`–`AWF1054` as placeholders, **replace with the actual next-free codes**.

```go
package ir

import "testing"

func reactLD(r *React, tools map[string]Tool) *LoadedDefinition {
	return makeLD(&Workflow{
		ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Tools:      tools,
		Graph:      NodeList{r},
	})
}

func okTools() map[string]Tool {
	return map[string]Tool{"check": {Description: "d", InputSchema: &JSONSchema{"type": "object"},
		Impl: ToolImpl{Run: "true", Container: "fin"}}}
}

func TestValidateReactToolsEmpty(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: nil, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1050", "react[0]")
}

func TestValidateReactToolUnknown(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"missing"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1051", "react[0]")
}

func TestValidateReactMaxTurnsNonPositive(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, MaxTurns: -1, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1052", "react[0]")
}

func TestValidateReactOutputSchemaReservesStopReason(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"},
		With:         RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &JSONSchema{"type": "object", "properties": map[string]any{"stop_reason": map[string]any{"type": "string"}}}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1053", "react[0]")
}

func TestValidateToolImplMissingContainer(t *testing.T) {
	tools := map[string]Tool{"check": {Description: "d", InputSchema: &JSONSchema{"type": "object"}, Impl: ToolImpl{Run: "true"}}}
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, tools)), "AWF1054", "tools.check")
}
```

- [ ] **Step 2: Run — expect FAIL** (no diagnostics emitted; `assertErrorAt` fails to find the codes).

Run: `go test -race ./ir/ -run TestValidateReact`
Expected: FAIL (expected diagnostic not found)

- [ ] **Step 3: Implement `ir/validate_tools.go`.** A new pass that walks `wf.Tools` and every `react:` node. (Mirror the existing pass signature in `ir/validate.go` — likely `func validateTools(ld *LoadedDefinition) []Diagnostic`.)

```go
package ir

import "fmt"

// reservedReactOutputFields are engine-injected siblings of a react: node's typed
// output; an output_schema may not declare them. One constant (the QuorumVerdictFields
// pattern). Spec §5/§6.1.
var reservedReactOutputFields = []string{"stop_reason"}

func validateTools(ld *LoadedDefinition) []Diagnostic {
	wf := ld.Workflow
	var diags []Diagnostic

	// Tool definitions.
	for name, tool := range wf.Tools {
		path := "tools." + name
		if tool.Impl.Container == "" {
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1054", Path: path,
				Message: fmt.Sprintf("tool %q impl must name a containers:-declared container", name)})
		} else if _, ok := wf.Containers[tool.Impl.Container]; !ok {
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1054", Path: path,
				Message: fmt.Sprintf("tool %q impl container %q is not declared in containers:", name, tool.Impl.Container)})
		}
		// input_schema floor + output_schema floor reuse the existing schema validator
		// (call the same helper gate/map output_schema validation uses).
	}

	// react: nodes — walk the graph for *React.
	WalkNodes(wf.Graph, func(n Node, path string) {
		r, ok := n.(*React)
		if !ok {
			return
		}
		if len(r.Tools) == 0 {
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1050", Path: path,
				Message: "react: tools must be non-empty (a react with no tools is an agent: step)"})
		}
		for _, tn := range r.Tools {
			if _, ok := wf.Tools[tn]; !ok {
				diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1051", Path: path,
					Message: fmt.Sprintf("react: tool %q is not declared in the top-level tools: map", tn)})
			}
		}
		if r.MaxTurns < 0 || (r.MaxTurns == 0 && false) { // 0 means "default 8"; reject negative only
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1052", Path: path,
				Message: "react: max_turns must be >= 1"})
		}
		if r.MaxTurns < 0 {
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1052", Path: path,
				Message: "react: max_turns must be >= 1"})
		}
		if r.OutputSchema != nil {
			if props, ok := (*r.OutputSchema)["properties"].(map[string]any); ok {
				for _, reserved := range reservedReactOutputFields {
					if _, clash := props[reserved]; clash {
						diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1053", Path: path,
							Message: fmt.Sprintf("react: output_schema may not declare reserved field %q", reserved)})
					}
				}
			}
		}
	})
	return diags
}
```

> Replace `WalkNodes`/`Diagnostic`/`Severity`/`Container` with the exact existing names (verify against `ir/walk.go`, `ir/diagnostic.go`, `ir/types.go`). Use the real next-free AWF codes. Clean up the redundant `MaxTurns` check left in for clarity — keep a single `if r.MaxTurns < 0` arm.

- [ ] **Step 4: Wire the pass** into `ir/validate.go`'s pass list (where `validatePrune`/`validateRefs` etc. are appended).

- [ ] **Step 5: Run — expect PASS.**

Run: `go test -race ./ir/ -run TestValidateReact && go test -race ./ir/ -run TestValidateToolImpl`
Expected: PASS

- [ ] **Step 6: Run the full `ir` suite + lint, then commit.**

```bash
go test -race ./ir/ && gofmt -l ir/
git add ir/validate_tools.go ir/validate.go ir/validate_tools_test.go
git commit -m "feat(ir): validate tools:/react: cross-refs + reserved fields (P3)"
```

### Task 1.5: Adapter gating + Ollama rejection at validate

**Files:**
- Modify: `ir/validate_tools.go` (extend the `*React` arm)
- Test: `ir/validate_tools_test.go` (append)

- [ ] **Step 1: Write the failing tests** — `react.with.uses` must resolve to a containerless+threaded adapter, and a `structured_output: ollama_format` config is rejected.

```go
func TestValidateReactRejectsNonAwfllm(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"},
		With: RawConfig{"uses": "anthropic/claude-code", "model": "m"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1055", "react[0]")
}

func TestValidateReactRejectsOllamaFormat(t *testing.T) {
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"},
		With: RawConfig{"uses": "awf/llm", "model": "m", "structured_output": "ollama_format"}}
	assertErrorAt(t, Validate(reactLD(r, okTools())), "AWF1056", "react[0]")
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./ir/ -run 'TestValidateReactRejects'`
Expected: FAIL

- [ ] **Step 3: Extend the `*React` arm** in `validate_tools.go`:

```go
		// Adapter gate: react: requires awf/llm (containerless+threaded), OpenAI-compat path.
		uses, _ := r.With["uses"].(string)
		if uses != "awf/llm" { // the only Containerless+Threaded adapter in v1
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1055", Path: path,
				Message: fmt.Sprintf("react: requires a containerless, threaded adapter (awf/llm); got uses=%q", uses)})
		}
		if so, _ := r.With["structured_output"].(string); so == "ollama_format" {
			diags = append(diags, Diagnostic{Severity: Error, Code: "AWF1056", Path: path,
				Message: "react: tools are not supported on the Ollama-native path (structured_output: ollama_format)"})
		}
```

> Note: if `react.with.uses` may name an `agents:` role (resolved later), the static check can't see through it. v1 keeps this a literal `awf/llm` check at load time; the run-start defensive gate (Phase 4, the `Caps.Containerless && Caps.Threaded` assertion) is the authoritative gate for the role case. Document this in a code comment.

- [ ] **Step 4: Run — expect PASS, then full suite + commit.**

```bash
go test -race ./ir/ && gofmt -l ir/
git add ir/validate_tools.go ir/validate_tools_test.go
git commit -m "feat(ir): react: adapter gating + ollama-path rejection (P3)"
```

### Task 1.6: `args`/`args_file` walkRefs carve-out + `react` producer + `stop_reason` accept arm

**Files:**
- Modify: `ir/validate_refs.go` (the walkRefs/checkRef arms + producer registration)
- Test: `ir/validate_refs_test.go` (append)

- [ ] **Step 1: Write the failing tests.** `{{ args.x }}`/`{{ args_file }}` inside a tool impl must NOT error; `{{ <react-id>.field }}` must resolve (producer registered); `{{ <react-id>.stop_reason }}` must be accepted.

```go
func TestValidateRefsArgsAllowedInToolImpl(t *testing.T) {
	tools := map[string]Tool{"check": {Description: "d", InputSchema: &JSONSchema{"type": "object"},
		Impl: ToolImpl{Run: "./v --f {{ args_file }} --x {{ args.x }}", Container: "fin"}}}
	r := &React{ID: "a", Prompt: "q", Tools: []string{"check"}, With: RawConfig{"uses": "awf/llm", "model": "m"}}
	assertNoErrorCode(t, Validate(reactLD(r, tools)), "AWF3001")
}

func TestValidateRefsReactOutputAddressable(t *testing.T) {
	r := &React{ID: "ans", Prompt: "q", Tools: []string{"check"},
		With:         RawConfig{"uses": "awf/llm", "model": "m"},
		OutputSchema: &JSONSchema{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}}}
	ld := makeLD(&Workflow{ID: "wf", Version: 1,
		Containers: map[string]Container{"fin": {Image: "oci://x@sha256:abc"}},
		Tools:      okTools(),
		Graph:      NodeList{r},
		Outputs:    map[string]string{"final": "{{ ans.answer }}", "why": "{{ ans.stop_reason }}"}})
	assertNoErrorCode(t, Validate(ld), "AWF3001")
	assertNoErrorCode(t, Validate(ld), "AWF4002")
}
```

- [ ] **Step 2: Run — expect FAIL** (`{{ ans.answer }}` is an unregistered producer → AWF3001; `args.*` may or may not error depending on whether the walk reaches impl bodies — confirm the failure reason).

Run: `go test -race ./ir/ -run 'TestValidateRefs(ArgsAllowed|ReactOutput)'`
Expected: FAIL

- [ ] **Step 3: Register the react producer** in `indexProducers` (mirror the `*Map` arm at `validate_refs.go:162-177`): for a `*React` node, register `producers[r.ID] = producer{path: <react[N] path>, kind: "react", schema: r.OutputSchema-plus-stop_reason}`. Add `stop_reason` as a synthetic string field so `{{ id.stop_reason }}` resolves. In `checkRef`'s producer-field resolution, the `react` producer accepts the schema's declared fields **plus** the reserved `stop_reason`.

```go
// in indexProducers, alongside the Map case:
	case *React:
		producers[v.ID] = producer{path: path, kind: "react", schema: v.OutputSchema}
```

> Then, where producer fields are checked (the step/non-aggregate case ~617), add: for `kind == "react"`, treat `stop_reason` as a valid field even if the schema doesn't declare it (the synthetic-field accept arm). For the `args`/`args_file` carve-out: the new walkRefs arm that descends into `tools[*].impl.run`/`exec` must treat `args` and `args_file` as allowed roots (the `prune.stop_when` precedent: don't static-type-check context-local roots, defer to runtime). The simplest correct form is to skip ref-validation of impl bodies (they are validated at runtime via the `toolImplScope`), matching `validate_prune.go:13-14`.

- [ ] **Step 4: Implement the carve-out** — in the producer-walk / ref-walk, ensure impl `run`/`exec` templates are either (a) not walked for refs, or (b) walked with `args`/`args_file` whitelisted. Choose (a) for v1 (prune precedent) and leave a comment citing `validate_prune.go:13-14`.

- [ ] **Step 5: Run — expect PASS, full suite, lint, commit.**

```bash
go test -race ./ir/ && gofmt -l ir/ && go vet ./ir/
git add ir/validate_refs.go ir/validate_refs_test.go
git commit -m "feat(ir): react producer + args/stop_reason ref carve-outs (P3)"
```

---

# Phase 2 — Journaling primitives (path + events + runstate + fold)

> All four edits are pure-additive and unit-testable with no engine run. Fold's default arm keeps old logs replayable.

### Task 2.1: `roundSep` + `RoundPath`/`ModelPath`/`ToolPath`

**Files:**
- Modify: `engine/path.go`
- Test: `engine/path_test.go` (append)

- [ ] **Step 1: Write the failing test.**

```go
func TestRoundPathHelpers(t *testing.T) {
	r := "react[0]"
	rp := RoundPath(r, 2)
	if rp != "react[0].round-2" {
		t.Fatalf("RoundPath = %q, want react[0].round-2", rp)
	}
	if ModelPath(rp) != "react[0].round-2.model" {
		t.Fatalf("ModelPath = %q", ModelPath(rp))
	}
	if ToolPath(rp, 1) != "react[0].round-2.tool-1" {
		t.Fatalf("ToolPath = %q", ToolPath(rp, 1))
	}
	// ParentPath already handles the new segments (trims the last '.'-segment).
	if ParentPath(ModelPath(rp)) != rp {
		t.Fatalf("ParentPath(model) = %q, want %q", ParentPath(ModelPath(rp)), rp)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined helpers).

Run: `go test -race ./engine/ -run TestRoundPathHelpers`
Expected: FAIL

- [ ] **Step 3: Implement** in `engine/path.go` — add `roundSep` to the const block and the three helpers (mirror `IterPath`/`AttemptPath`/`ItemStepPath`):

```go
// in the separator const block (alongside iterSep/attemptSep/itemSep):
	roundSep = ".round-"

// RoundPath appends a per-round suffix to a react node's runtime path:
//   RoundPath("react[0]", 2) → "react[0].round-2"   (round is 1-based)
func RoundPath(reactPath string, round int) string {
	return reactPath + roundSep + strconv.Itoa(round)
}

// ModelPath is the per-round synthetic model leaf: "<round>.model".
func ModelPath(roundPath string) string { return roundPath + ".model" }

// ToolPath is the per-round, per-call tool impl leaf: "<round>.tool-J"
// (J = the tool_call's stable Index).
func ToolPath(roundPath string, j int) string {
	return roundPath + ".tool-" + strconv.Itoa(j)
}
```

- [ ] **Step 4: Run — expect PASS.**

Run: `go test -race ./engine/ -run TestRoundPathHelpers`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add engine/path.go engine/path_test.go
git commit -m "feat(engine): add roundSep + RoundPath/ModelPath/ToolPath (P3)"
```

### Task 2.2: `EventReactRound` + `ReactRoundData`

**Files:**
- Modify: `engine/events.go`
- Test: `engine/events_test.go` (append)

- [ ] **Step 1: Write the failing round-trip test** (template on `TestLoopIterDataRoundTrip`):

```go
func TestReactRoundDataRoundTrip(t *testing.T) {
	in := ReactRoundData{N: 3}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"n":3}` {
		t.Fatalf("on-wire = %s, want {\"n\":3}", b)
	}
	var out ReactRoundData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.N != 3 {
		t.Fatalf("N = %d, want 3", out.N)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined `ReactRoundData`).

Run: `go test -race ./engine/ -run TestReactRoundDataRoundTrip`
Expected: FAIL

- [ ] **Step 3: Implement** in `engine/events.go` — add a const (its own block, like `EventGateAttempt`) and the payload struct (shape-identical to `LoopIterData`; `finish_reason` lives on the `.model` leaf, NOT here):

```go
const (
	// P3 A3. Committed by the react executor (engine/react.go) at the END of each
	// round — AFTER the round's .model leaf and every dispatched .tool-J leaf have
	// committed. A pure {N} round cursor; finish_reason lives on the .model leaf.
	// Durability: Append+Sync (gate-style), NOT loop.iter's fsync-riding append,
	// because tool side-effects are not first-run-equivalent on resume (spec §4.1).
	EventReactRound = "react.round"
)

// ReactRoundData is the payload of a react.round marker. N is 1-based.
type ReactRoundData struct {
	N int `json:"n"`
}
```

- [ ] **Step 4: Run — expect PASS.**

Run: `go test -race ./engine/ -run TestReactRoundDataRoundTrip`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add engine/events.go engine/events_test.go
git commit -m "feat(engine): add react.round event + ReactRoundData (P3)"
```

### Task 2.3: `RunState.ReactRounds` + `ReactRoundRecord` + accessors

**Files:**
- Modify: `engine/runstate.go`
- Test: `engine/runstate_test.go` (append)

- [ ] **Step 1: Write the failing test** (mirror the gate-attempts accessor tests):

```go
func TestReactRoundsAccessors(t *testing.T) {
	rs := NewRunState("r", "d", nil)
	if got := rs.LookupReactRounds("react[0]"); got != nil {
		t.Fatalf("empty lookup = %v, want nil", got)
	}
	rs.RecordReactRound("react[0]", ReactRoundRecord{N: 1})
	rs.RecordReactRound("react[0]", ReactRoundRecord{N: 2})
	got := rs.LookupReactRounds("react[0]")
	if len(got) != 2 || got[0].N != 1 || got[1].N != 2 {
		t.Fatalf("rounds = %+v, want [{1} {2}]", got)
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./engine/ -run TestReactRoundsAccessors`
Expected: FAIL

- [ ] **Step 3: Implement.** Add the `ReactRoundRecord` struct (near `AttemptResult`), the `ReactRounds` field (near `GateAttempts`), the constructor literal entry, and the two accessors (copy `LookupGateAttempts`/`RecordGateAttempt` verbatim with the rename). Also add `ReactRounds` to the `mu` doc-comment's guarded-maps list.

```go
// near AttemptResult / MapItemRecord:
type ReactRoundRecord struct {
	N int // 1-based round number; finish_reason is read from the .model leaf (spec §4.1)
}

// in the RunState struct, alongside GateAttempts:
	// ReactRounds records committed rounds per react node (react[N] path → ordered
	// slice, oldest first). startK := len(ReactRounds[R])+1 is the resume cursor.
	ReactRounds map[string][]ReactRoundRecord

// in NewRunState's literal, alongside GateAttempts:
		ReactRounds: map[string][]ReactRoundRecord{},

// accessors (copy of LookupGateAttempts/RecordGateAttempt):
func (rs *RunState) LookupReactRounds(r string) []ReactRoundRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.ReactRounds[r] // READ-ONLY: callers must not mutate the returned slice
}

func (rs *RunState) RecordReactRound(r string, rr ReactRoundRecord) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.ReactRounds[r] = append(rs.ReactRounds[r], rr)
}
```

- [ ] **Step 4: Run — expect PASS.**

Run: `go test -race ./engine/ -run TestReactRoundsAccessors`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add engine/runstate.go engine/runstate_test.go
git commit -m "feat(engine): RunState.ReactRounds + accessors (P3)"
```

### Task 2.4: `EventReactRound` fold arm + pre-alloc

**Files:**
- Modify: `engine/fold.go`
- Test: `engine/fold_test.go` (append)

- [ ] **Step 1: Write the failing test** (template on `TestFold_GateAttemptPopulatesAttempts`; every event list must start with `EventRunStarted`):

```go
func TestFold_ReactRoundsAppend(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventReactRound, Path: "react[0]", Data: marshalOrFatal(t, ReactRoundData{N: 1})},
		{Seq: 3, TS: fixedTS, Type: EventReactRound, Path: "react[0]", Data: marshalOrFatal(t, ReactRoundData{N: 2})},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rounds := rs.ReactRounds["react[0]"]
	if len(rounds) != 2 || rounds[0].N != 1 || rounds[1].N != 2 {
		t.Fatalf("ReactRounds = %+v, want [{1} {2}]", rounds)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (the default arm ignores `react.round` → `ReactRounds` stays empty; assertion fails).

Run: `go test -race ./engine/ -run TestFold_ReactRoundsAppend`
Expected: FAIL ("ReactRounds = [] want [{1} {2}]")

- [ ] **Step 3: Implement.** Add the pre-alloc in Fold's `make` block (near `GateAttempts`) and the fold arm (the *simplest* of the four — no blob deref):

```go
// in the rs.X = make(...) pre-alloc block (~fold.go:83-92):
	rs.ReactRounds = make(map[string][]ReactRoundRecord, len(events)/16)

// new case in the Fold switch (near the EventGateAttempt arm):
		case EventReactRound:
			var d ReactRoundData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventReactRound, e.Seq, e.Path, err)
			}
			rs.ReactRounds[e.Path] = append(rs.ReactRounds[e.Path], ReactRoundRecord{N: d.N})
```

- [ ] **Step 4: Run — expect PASS.** Also run the full engine suite to confirm no regression (the default-arm additivity guarantee).

Run: `go test -race ./engine/ -run TestFold`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
git add engine/fold.go engine/fold_test.go
git commit -m "feat(engine): fold react.round into RunState.ReactRounds (P3)"
```

---

# Phase 3 — Adapter seam + transport tool wiring

### Task 3.1: `agent.ToolLoopRunner` optional interface + `ToolLoop*` types

**Files:**
- Modify: `agent/adapter.go` (interface, next to `ResumePreflighter`)
- Create: `agent/toolloop.go` (the `ToolLoop*` value types)
- Test: `agent/toolloop_test.go`

- [ ] **Step 1: Write the failing test** — a compile-level test that the interface exists and the types round-trip:

```go
package agent

import "testing"

func TestToolLoopTypesExist(t *testing.T) {
	inv := ToolLoopInvocation{
		NodePath: "react[0].round-1.model",
		Uses:     "awf/llm",
		Messages: []ReactTurn{{Role: "user", Content: "hi"}},
		Tools:    []ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}
	res := ToolLoopResult{
		Text:         "",
		ToolCalls:    []ToolCall{{Index: 0, ID: "call_1", Name: "check", Arguments: `{"x":1}`}},
		FinishReason: "tool_calls",
	}
	if inv.Messages[0].Role != "user" || res.ToolCalls[0].ID != "call_1" {
		t.Fatal("type wiring wrong")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined types).

Run: `go test -race ./agent/ -run TestToolLoopTypesExist`
Expected: FAIL

- [ ] **Step 3: Implement `agent/toolloop.go`** (the engine-owned, tool-aware message shape — pins the field layout the spec §10 left open):

```go
package agent

// ToolCall is one model-emitted tool invocation. Arguments is the RAW model-emitted
// JSON string, stored verbatim (the §4.5 determinism invariant — never reserialized).
type ToolCall struct {
	Index     int    `json:"index"` // stable position; the J in react[N].round-K.tool-J
	ID        string `json:"id"`    // matches the tool-role message's tool_call_id
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ReactTurn is one message in an engine-owned tool-loop conversation. Role is
// "user" | "assistant" | "tool". An assistant turn may carry ToolCalls; a tool turn
// carries ToolCallID + Content (the result). Distinct from ThreadTurn (continues:)
// which cannot represent tool_calls / tool-role messages.
type ReactTurn struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`  // assistant turns only; OMIT (not []) when none
	ToolCallID string     `json:"tool_call_id,omitempty"`// tool turns only
}

// ToolDef is a tool offered to the model (name + description + parameters schema).
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolLoopInvocation is ONE model call with tools attached + the full prior message
// history. The engine (runReact) owns the history; the adapter just executes the call.
type ToolLoopInvocation struct {
	NodePath     string         `json:"node_path"`
	Uses         string         `json:"uses"`
	With         RawConfigAlias `json:"with,omitempty"` // see note
	Messages     []ReactTurn    `json:"messages"`
	Tools        []ToolDef      `json:"tools"`
	OutputSchema *JSONSchemaAlias `json:"output_schema,omitempty"`
	Env          SecretEnv      `json:"-"`
}

// ToolLoopResult is the model's response for one call.
type ToolLoopResult struct {
	Text         string     `json:"text"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason"`
	Metrics      *MetricSet `json:"metrics,omitempty"`
}
```

> Replace `RawConfigAlias`/`JSONSchemaAlias` with the real types `agent` already uses for `With`/`OutputSchema` on `AgentInvocation` (`ir.RawConfig`, `*ir.JSONSchema`) — keep imports consistent with `agent/types.go`.

- [ ] **Step 4: Add the interface** in `agent/adapter.go` next to `ResumePreflighter`:

```go
// ToolLoopRunner is implemented only by adapters that can run an engine-mediated
// tool loop (Caps.Containerless && Caps.Threaded — in v1, only awf/llm). It is an
// OPTIONAL interface (the ResumePreflighter pattern), NOT part of the Adapter seam,
// so the other four adapters are untouched. runReact obtains it via a type assertion.
type ToolLoopRunner interface {
	RunToolLoop(ctx context.Context, inv ToolLoopInvocation) (ToolLoopResult, error)
}
```

- [ ] **Step 5: Run — expect PASS, then commit.**

```bash
go test -race ./agent/ -run TestToolLoop && gofmt -l agent/
git add agent/adapter.go agent/toolloop.go agent/toolloop_test.go
git commit -m "feat(agent): ToolLoopRunner optional interface + ToolLoop types (P3)"
```

### Task 3.2: `DerivedAdapter.RunToolLoop` forwarding

**Files:**
- Modify: `agent/derived.go`
- Test: `agent/derived_test.go` (append)

- [ ] **Step 1: Write the failing test** — a base adapter implementing `ToolLoopRunner`, wrapped by `DerivedAdapter`, must forward (and merge the role `with:`). Use a tiny stub base.

```go
type stubToolLoop struct {
	Adapter // embed a no-op base or use an existing fake; only RunToolLoop matters here
	gotWith ir.RawConfig
}

func (s *stubToolLoop) RunToolLoop(ctx context.Context, inv ToolLoopInvocation) (ToolLoopResult, error) {
	s.gotWith = inv.With
	return ToolLoopResult{FinishReason: "stop", Text: "ok"}, nil
}

func TestDerivedAdapterForwardsRunToolLoop(t *testing.T) {
	base := &stubToolLoop{ /* ... */ }
	d := NewDerivedAdapter("role", base, ir.RawConfig{"model": "role-model"})
	runner, ok := any(d).(ToolLoopRunner)
	if !ok {
		t.Fatal("DerivedAdapter does not satisfy ToolLoopRunner")
	}
	res, err := runner.RunToolLoop(context.Background(), ToolLoopInvocation{With: ir.RawConfig{"prompt": "x"}})
	if err != nil || res.Text != "ok" {
		t.Fatalf("forward failed: %v %v", res, err)
	}
	if base.gotWith["model"] != "role-model" {
		t.Fatalf("role with: not merged: %v", base.gotWith)
	}
}
```

> Adapt the stub to whatever minimal `Adapter` satisfaction the test needs (you may reuse an existing test fake). The key assertions: `DerivedAdapter` satisfies `ToolLoopRunner` and merges the role `with:` (step-wins).

- [ ] **Step 2: Run — expect FAIL** (DerivedAdapter has no `RunToolLoop`).

Run: `go test -race ./agent/ -run TestDerivedAdapterForwardsRunToolLoop`
Expected: FAIL

- [ ] **Step 3: Implement** in `agent/derived.go` (copy the `PreflightResume` forwarding shape verbatim):

```go
func (d *DerivedAdapter) RunToolLoop(ctx context.Context, inv ToolLoopInvocation) (ToolLoopResult, error) {
	runner, ok := d.base.(ToolLoopRunner)
	if !ok {
		return ToolLoopResult{}, fmt.Errorf("base adapter %q for role %q does not implement agent.ToolLoopRunner", d.base.Ref(), d.roleName)
	}
	inv.With = d.merge(inv.With)
	return runner.RunToolLoop(ctx, inv)
}
```

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./agent/ -run TestDerivedAdapter && gofmt -l agent/
git add agent/derived.go agent/derived_test.go
git commit -m "feat(agent): DerivedAdapter forwards RunToolLoop to base (P3 C2)"
```

### Task 3.3: Prompt-exempt `ValidateConfig` variant

**Files:**
- Modify: `agent/awfllm/validate.go`
- Test: `agent/awfllm/validate_test.go` (append)

- [ ] **Step 1: Write the failing test** — a react `with:` (no `prompt`) must pass the prompt-exempt validation but still reject a `tools` key and keep all other checks.

```go
func TestValidateConfigToolLoopExemptsPrompt(t *testing.T) {
	a := &Adapter{env: agent.SecretEnv{"OPENAI_API_KEY": "x"}}
	// no prompt key:
	if err := a.validateConfigForToolLoop(ir.RawConfig{"model": "m"}); err != nil {
		t.Fatalf("prompt-exempt validate rejected a valid react with:: %v", err)
	}
	// tools key still rejected:
	if err := a.validateConfigForToolLoop(ir.RawConfig{"model": "m", "tools": []any{}}); err == nil {
		t.Fatal("tools with-key should still be rejected")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined method).

Run: `go test -race ./agent/awfllm/ -run TestValidateConfigToolLoopExemptsPrompt`
Expected: FAIL

- [ ] **Step 3: Refactor** `ValidateConfig` — extract everything except the `requireNonEmptyString(with, keyPrompt)` check into a shared helper, and add the exempt variant. Do NOT delete the prompt check from `ValidateConfig` (agent: still needs it).

```go
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	if err := a.validateConfigCommon(with); err != nil {
		return err
	}
	return requireNonEmptyString(with, keyPrompt)
}

// validateConfigForToolLoop is ValidateConfig minus the prompt requirement: react:
// supplies the initial user turn at the step level; the engine owns the messages
// array. The rejectedKeys guard (incl. "tools") and all other checks still apply.
func (a *Adapter) validateConfigForToolLoop(with ir.RawConfig) error {
	return a.validateConfigCommon(with)
}

// validateConfigCommon holds the shared body (rejectedKeys, allowedKeys, model,
// base_url/api_key_env/system_prompt/temperature/max_tokens/structured_output/
// tls_insecure types, the api-key-env presence policy) — everything except prompt.
func (a *Adapter) validateConfigCommon(with ir.RawConfig) error {
	// ...move the current ValidateConfig body here, minus the keyPrompt require...
}
```

> Move the existing body carefully; keep `keyPrompt` in `allowedKeys` (agent: still uses it; for react: an accidental `prompt` in `with:` is simply ignored by the tool-loop path, or you may add an explicit reject — keep v1 lenient).

- [ ] **Step 4: Run — expect PASS, full awfllm suite, commit.**

```bash
go test -race ./agent/awfllm/ && gofmt -l agent/awfllm/
git add agent/awfllm/validate.go agent/awfllm/validate_test.go
git commit -m "feat(awfllm): prompt-exempt ValidateConfig variant for react: (P3)"
```

### Task 3.4: Transport — attach tools + parse `tool_calls` (OpenAI path)

**Files:**
- Modify: `agent/awfllm/transport.go` (a tool-aware single-call path)
- Test: `agent/awfllm/transport_test.go` (append, using the injected `httpClient` fake)

- [ ] **Step 1: Write the failing test.** The existing transport tests inject a fake `*http.Client` returning a canned SSE stream. Add a test that feeds a streamed `tool_calls` response and asserts the parsed `ToolCall{Index,ID,Name,Arguments}` (Arguments verbatim) + `finish_reason == "tool_calls"`. (Model the canned stream on the existing transport test fixtures.)

```go
func TestStreamOpenAIParsesToolCalls(t *testing.T) {
	// canned SSE: an assistant turn that finishes with a tool_call to "check"
	// with arguments {"iban":"DE89"} and finish_reason "tool_calls".
	a := newTestAdapterWithCannedStream(t, toolCallSSEFixture)
	res, err := a.runOneToolCall(context.Background(), testReqConfig(), []agent.ReactTurn{{Role: "user", Content: "q"}},
		[]agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatalf("runOneToolCall: %v", err)
	}
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 1 {
		t.Fatalf("res = %+v", res)
	}
	tc := res.ToolCalls[0]
	if tc.Name != "check" || tc.Arguments != `{"iban":"DE89"}` || tc.ID == "" {
		t.Fatalf("tool_call = %+v", tc)
	}
}
```

> Name the canned-stream + `reqConfig` helpers to match the existing transport test conventions. Build `toolCallSSEFixture` from the chunk shape openai-go emits for streamed tool calls (an assistant delta with `tool_calls` fragments + a final `finish_reason: "tool_calls"` chunk).

- [ ] **Step 2: Run — expect FAIL** (undefined `runOneToolCall`).

Run: `go test -race ./agent/awfllm/ -run TestStreamOpenAIParsesToolCalls`
Expected: FAIL

- [ ] **Step 3: Implement `runOneToolCall`** in `transport.go` — a tool-aware variant of `streamOpenAI` that: builds `messages` from `[]agent.ReactTurn` (user/assistant-with-tool_calls/tool roles), attaches `params.Tools`, drives a `ChatCompletionAccumulator`, and reads the authoritative `acc.Choices[0].Message.ToolCalls` after the stream (more reliable than per-chunk `JustFinishedToolCall` under parallel calls). Guard `len(chunk.Choices) > 0`.

```go
func (a *Adapter) runOneToolCall(ctx context.Context, cfg reqConfig, msgs []agent.ReactTurn, tools []agent.ToolDef) (agent.ToolLoopResult, error) {
	client := openai.NewClient(
		option.WithBaseURL(cfg.BaseURL), option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(a.clientFor(cfg.TLSInsecure)), option.WithMaxRetries(0),
	)
	messages := buildOpenAIMessages(cfg.SystemPrompt, msgs) // user/assistant(+tool_calls)/tool
	params := openai.ChatCompletionNewParams{
		Model:         cfg.Model,
		Messages:      messages,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	for _, td := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        td.Name,
			Description: param.NewOpt(td.Description),
			Strict:      param.NewOpt(true),
			Parameters:  shared.FunctionParameters(td.InputSchema),
		}))
	}
	// ToolChoice left unset → "auto" (SDK default when Tools non-empty).

	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var acc openai.ChatCompletionAccumulator
	var usage usageRec
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
			usage.Input = int(chunk.Usage.PromptTokens)
			usage.Output = int(chunk.Usage.CompletionTokens)
			usage.CacheRead = int(chunk.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	if err := stream.Err(); err != nil {
		return agent.ToolLoopResult{}, classifyOpenAIErr(err)
	}
	if len(acc.Choices) == 0 {
		return agent.ToolLoopResult{}, fmt.Errorf("awfllm: tool-loop response had no choices")
	}
	msg := acc.Choices[0].Message
	res := agent.ToolLoopResult{
		Text:         msg.Content,
		FinishReason: string(acc.Choices[0].FinishReason),
		Metrics:      metricsFrom(usage, cfg.Model), // reuse the existing usage→MetricSet helper
	}
	for _, tc := range msg.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, agent.ToolCall{
			Index: int(tc.Index), ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return res, nil
}
```

> Verify the exact field names on `acc.Choices[0].Message.ToolCalls[i]` (`.ID`, `.Index`, `.Function.Name`, `.Function.Arguments`) against openai-go v3.39.0. Implement `buildOpenAIMessages` to map `ReactTurn` → `openai.ChatCompletionMessageParamUnion` (user → `UserMessage`, tool → `ToolMessage(content, ToolCallID)`, assistant-with-tool_calls → reconstruct via the stored `tool_calls`; **omit** `tool_calls` entirely when empty). Add the `…/v3/shared` and `…/v3/packages/param` imports.

- [ ] **Step 4: Run — expect PASS, full awfllm suite, lint, commit.**

```bash
go test -race ./agent/awfllm/ && gofmt -l agent/awfllm/ && go vet ./agent/awfllm/
git add agent/awfllm/transport.go agent/awfllm/transport_test.go
git commit -m "feat(awfllm): attach tools + parse tool_calls on the OpenAI path (P3)"
```

### Task 3.5: `*awfllm.Adapter.RunToolLoop` + compile-time assertion

**Files:**
- Modify: `agent/awfllm/adapter.go`
- Test: `agent/awfllm/adapter_test.go` (append)

- [ ] **Step 1: Write the failing test** — the adapter satisfies `ToolLoopRunner` and one round returns the parsed result (drive it through the injected fake client).

```go
func TestAdapterRunToolLoopOneRound(t *testing.T) {
	var _ agent.ToolLoopRunner = (*Adapter)(nil) // compile-time
	a := newTestAdapterWithCannedStream(t, toolCallSSEFixture)
	res, err := a.RunToolLoop(context.Background(), agent.ToolLoopInvocation{
		Uses: "awf/llm",
		With: ir.RawConfig{"model": "m", "base_url": "http://x"},
		Messages: []agent.ReactTurn{{Role: "user", Content: "q"}},
		Tools:    []agent.ToolDef{{Name: "check", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil || res.FinishReason != "tool_calls" {
		t.Fatalf("RunToolLoop: %+v %v", res, err)
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./agent/awfllm/ -run TestAdapterRunToolLoopOneRound`
Expected: FAIL

- [ ] **Step 3: Implement `RunToolLoop`** — resolve `reqConfig` from `inv.With` via the prompt-exempt validate + the existing config parser, then call `runOneToolCall`. Add the compile-time assertion.

```go
// var _ agent.ToolLoopRunner = (*Adapter)(nil)  // next to the existing var _ agent.Adapter

func (a *Adapter) RunToolLoop(ctx context.Context, inv agent.ToolLoopInvocation) (agent.ToolLoopResult, error) {
	if err := a.validateConfigForToolLoop(inv.With); err != nil {
		return agent.ToolLoopResult{}, err
	}
	cfg, err := a.reqConfigFrom(inv.With) // the existing with:→reqConfig parser
	if err != nil {
		return agent.ToolLoopResult{}, err
	}
	if cfg.StructuredOutput == soOllamaFormat {
		return agent.ToolLoopResult{}, fmt.Errorf("awfllm: react: not supported on the Ollama-native path")
	}
	return a.runOneToolCall(ctx, cfg, inv.Messages, inv.Tools)
}
```

> Use the real name of the `with:`→`reqConfig` constructor (find it in `agent/awfllm/config.go`).

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./agent/awfllm/ && gofmt -l agent/awfllm/
git add agent/awfllm/adapter.go agent/awfllm/adapter_test.go
git commit -m "feat(awfllm): implement ToolLoopRunner.RunToolLoop (P3)"
```

---

# Phase 4 — `runReact` engine handler

> This is the load-bearing phase. It composes Phase 2 (journaling), Phase 3 (the model call), and the reduce-style synthesized tool dispatch. Build it inside-out: scope wrapper → one round (model leaf) → tool dispatch → the round loop → terminal commit → interpNode wiring.

### Task 4.1: `toolImplScope` wrapper

**Files:**
- Create: `engine/react_scope.go`
- Test: `engine/react_scope_test.go`

- [ ] **Step 1: Write the failing test** — `{{ args_file }}` and `{{ args.x }}` resolve; everything else delegates to the base scope; an unknown root still errors via the base.

```go
func TestToolImplScopeResolvesArgs(t *testing.T) {
	rs := NewRunState("r", "d", map[string]any{"q": "hi"})
	wf := &ir.Workflow{ID: "wf", Version: 1}
	s := newToolImplScope(rs, wf, "react[0].round-1.tool-0", "/awf/args/r1-t0.json", map[string]any{"x": 7, "obj": map[string]any{}})

	v, err := s.Resolve(mustRef(t, "args_file"))
	if err != nil || v != "/awf/args/r1-t0.json" {
		t.Fatalf("args_file = %v, %v", v, err)
	}
	v, err = s.Resolve(mustRef(t, "args.x"))
	if err != nil || v != 7 {
		t.Fatalf("args.x = %v, %v", v, err)
	}
	// non-scalar arg is absent (best-effort): args.obj → unresolved
	if _, err := s.Resolve(mustRef(t, "args.obj")); err == nil {
		t.Fatal("args.obj (non-scalar) should be unresolved")
	}
	// input.* delegates to base
	v, err = s.Resolve(mustRef(t, "input.q"))
	if err != nil || v != "hi" {
		t.Fatalf("input.q via base = %v, %v", v, err)
	}
}
```

> `mustRef` parses a ref string via the `template` package — find the existing test helper or build it from `template.ParseRef`.

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./engine/ -run TestToolImplScopeResolvesArgs`
Expected: FAIL

- [ ] **Step 3: Implement** `engine/react_scope.go` (the `reduceTemplateScope` intercept-then-delegate pattern):

```go
package engine

import "github.com/valbaudo/awf/template"

// toolImplScope wraps a base *Scope, intercepting the two react tool-impl roots
// (args_file, args.<field>) and delegating everything else to base. NOT an edit to
// Scope.Resolve (so args.* never leaks into general workflow scope). Spec §3.3.
type toolImplScope struct {
	base     *Scope
	argsFile string         // the per-call container path of the staged verbatim args
	args     map[string]any // parsed args (best-effort); only scalar leaves are served
}

func newToolImplScope(rs *RunState, wf *ir.Workflow, ctxPath, argsFile string, args map[string]any) *toolImplScope {
	return &toolImplScope{base: NewScope(rs, wf, ctxPath), argsFile: argsFile, args: args}
}

func (s *toolImplScope) Resolve(ref *template.Ref) (any, error) {
	g := ref.Segments
	if len(g) == 1 && !g[0].IsIndex && g[0].Ident == "args_file" {
		return s.argsFile, nil
	}
	if len(g) == 2 && !g[0].IsIndex && g[0].Ident == "args" && !g[1].IsIndex {
		if v, ok := s.args[g[1].Ident]; ok && isScalar(v) {
			return v, nil
		}
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "args.%s is not a bound scalar (use {{ args_file }})", g[1].Ident)
	}
	return s.base.Resolve(ref)
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, int, int64, json.Number:
		return true
	default:
		return false
	}
}
```

> Verify `template.Ref`/`Segments`/`Ident`/`IsIndex`/`EvalErrf`/`EvalCodeRefUnresolved` names against the `template` package (they appear in `prune.go`'s `bestScope`). Confirm `isScalar` covers how JSON numbers arrive after `json.Unmarshal` into `map[string]any` (they are `float64`).

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./engine/ -run TestToolImplScope && gofmt -l engine/
git add engine/react_scope.go engine/react_scope_test.go
git commit -m "feat(engine): toolImplScope wrapper for args/args_file (P3)"
```

### Task 4.2: `runReact` — single round, no tools (natural stop)

**Files:**
- Create: `engine/react.go`
- Modify: `engine/interpreter.go` (the `*ir.React` interpNode arm)
- Test: `engine/react_test.go`

> Build the simplest path first: a model that answers immediately (`finish_reason != "tool_calls"`), one round, terminal commit. No tool dispatch yet.

- [ ] **Step 1: Write the failing test** — drive `runReact` with a fake `ToolLoopRunner` scripted to return `{Text:"42", FinishReason:"stop"}`; assert the terminal `node.completed` at `react[0]` with output `{answer:"42"?}`/`{text:"42", stop_reason:"stop"}` and `ReactRounds["react[0]"]` length 1. Use the engine unit-test harness (a `*RunState`, in-mem log+blobs, a fake registry). Model it on an existing `engine/*_test.go` that drives a single handler.

```go
func TestRunReactNaturalStopOneRound(t *testing.T) {
	h := newReactTestHarness(t, /* react node with output_schema {answer} */, scriptedToolLoop{
		{Text: `{"answer":"42"}`, FinishReason: "stop"},
	})
	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: %v %v", oc, err)
	}
	nr, ok := h.rs.LookupCompleted("react[0]")
	if !ok {
		t.Fatal("react[0] not committed")
	}
	if nr.Outputs["answer"] != "42" || nr.Outputs["stop_reason"] != "stop" {
		t.Fatalf("terminal output = %v", nr.Outputs)
	}
	if len(h.rs.LookupReactRounds("react[0]")) != 1 {
		t.Fatalf("rounds = %d, want 1", len(h.rs.LookupReactRounds("react[0]")))
	}
}
```

> You will write `newReactTestHarness` + `scriptedToolLoop` (a fake satisfying `agent.ToolLoopRunner` with a per-call script). Keep them in `engine/react_test.go`. The harness wires a `RunState`, `state.NewInMemoryLog`, `state.NewInMemoryBlobs`, an `agent.Registry` with the scripted runner, and calls `runReactWithContext`.

- [ ] **Step 2: Run — expect FAIL** (undefined `runReact`).

Run: `go test -race ./engine/ -run TestRunReactNaturalStopOneRound`
Expected: FAIL

- [ ] **Step 3: Implement the skeleton** `engine/react.go` — entry short-circuit, the round loop (model leaf + terminate decision), terminal commit. (Tool dispatch is a stub that errors if reached — added in 4.3.)

```go
package engine

func runReact(ctx context.Context, r *ir.React, reactPath string, wf *ir.Workflow,
	runstate *RunState, resolver agent.Resolver, log state.Log, blobs state.Blobs, clk clock.Clock, tap io.Writer) (Outcome, error) {
	return runReactWithContext(ctx, r, reactPath, interpreterContext{
		wf: wf, runstate: runstate, resolver: resolver, log: log, blobs: blobs, clk: clk, tap: tap,
	})
}

func runReactWithContext(ctx context.Context, r *ir.React, reactPath string, ictx interpreterContext) (Outcome, error) {
	// Terminal short-circuit (reduce.go:150 precedent).
	if _, done := ictx.runstate.LookupCompleted(reactPath); done {
		return OutcomeOK, nil
	}
	runner, err := toolLoopRunnerFor(r, ictx) // resolves adapter + asserts ToolLoopRunner + Caps gate
	if err != nil {
		return "", err
	}
	maxTurns := r.MaxTurns
	if maxTurns == 0 {
		maxTurns = 8
	}
	startK := len(ictx.runstate.LookupReactRounds(reactPath)) + 1
	msgs, err := replayMessages(r, reactPath, ictx, startK) // rebuild history from committed leaves (4.4)
	if err != nil {
		return "", err
	}
	for k := startK; k <= maxTurns; k++ {
		roundPath := RoundPath(reactPath, k)
		modelPath := ModelPath(roundPath)

		// 1. Model leaf, guarded (reduce.go:150 + spec §4.3 execute step 1).
		var mr ToolLoopRoundResult
		if nr, ok := ictx.runstate.LookupCompleted(modelPath); ok {
			mr = roundResultFromLeaf(nr) // read tool_calls/text/finish_reason back; DO NOT re-sample
		} else {
			res, err := runner.RunToolLoop(ctx, buildToolLoopInvocation(r, modelPath, msgs))
			if err != nil {
				return "", fmt.Errorf("engine.runReact: model call at %q: %w", modelPath, err)
			}
			mr = toRoundResult(res)
			if err := commitModelLeaf(ictx, modelPath, mr); err != nil {
				return "", err
			}
		}
		appendAssistantTurn(&msgs, mr) // omit tool_calls when none (4.4 / M5)

		// 2. Terminate? (decide BEFORE dispatching any tool — spec §4.3 step 2)
		if mr.FinishReason != "tool_calls" {
			return commitTerminal(ictx, r, reactPath, mr, "stop")
		}
		if k == maxTurns {
			return commitTerminal(ictx, r, reactPath, mr, "max_turns")
		}

		// 3. Dispatch tools (added in Task 4.3).
		if err := dispatchRoundTools(ctx, r, roundPath, mr, &msgs, ictx); err != nil {
			return "", err
		}

		// 4. Close the round: react.round marker (Append+Sync), then Record (gate.go:161-170).
		if err := closeRound(ictx, reactPath, k); err != nil {
			return "", err
		}
	}
	// Unreachable: the k==maxTurns branch returns inside the loop.
	return "", fmt.Errorf("engine.runReact: %q fell through round loop", reactPath)
}
```

> For Task 4.2 only, implement: `toolLoopRunnerFor` (resolve `r.With["uses"]`, `resolver.Lookup`, assert `agent.ToolLoopRunner`, check `Caps().Containerless && Caps().Threaded` — error otherwise), `buildToolLoopInvocation`, `commitModelLeaf` (Commit a synthetic leaf with `{text, finish_reason, tool_calls:[...]}`), `commitTerminal` (build `{...schema fields or text..., stop_reason}` and Commit at `reactPath`), `appendAssistantTurn`, `replayMessages` (returns the initial `[user: r.Prompt]` when `startK==1`; full replay added in 4.4), and stub `dispatchRoundTools`/`closeRound` (4.3). Add the `interpNode` arm:

```go
// in engine/interpreter.go interpNode switch:
	case *ir.React:
		return runReactWithContext(ctx, v, ir.PathFor(parent, "react", "", idx), ictx)
```

> `ir.PathFor(parent, "react", "", idx)` — keyword "react", **empty id** (control-node form), so the path is `react[N]`.

- [ ] **Step 4: Run — expect PASS.**

Run: `go test -race ./engine/ -run TestRunReactNaturalStopOneRound`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
go test -race ./engine/ && gofmt -l engine/ && go vet ./engine/
git add engine/react.go engine/interpreter.go engine/react_test.go
git commit -m "feat(engine): runReact natural-stop single round + interpNode arm (P3)"
```

### Task 4.3: Tool dispatch within a round (synthesized CodeStep + verbatim args + failure feedback)

**Files:**
- Modify: `engine/react.go` (implement `dispatchRoundTools`, `closeRound`)
- Test: `engine/react_test.go` (append)

- [ ] **Step 1: Write the failing test** — a model that calls one tool, then answers. The fake runner scripts round 1 = `{tool_calls:[{check,{"x":1}}], finish_reason:"tool_calls"}`, round 2 = `{Text:"done", finish_reason:"stop"}`. The fake backend programs the tool impl's exec to return a result. Assert: `react[0].round-1.tool-0` committed; the round-1 marker committed; the tool result appears as a `tool` message in round 2's invocation; terminal committed.

```go
func TestRunReactDispatchesToolThenAnswers(t *testing.T) {
	h := newReactTestHarness(t, /* react with tool "check" */, scriptedToolLoop{
		{ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"x":1}`}}, FinishReason: "tool_calls"},
		{Text: `{"answer":"done"}`, FinishReason: "stop"},
	})
	h.programTool("check", container.ExecResult{ExitCode: 0, Stdout: []byte("RESULT-OK")})
	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: %v %v", oc, err)
	}
	if _, ok := h.rs.LookupCompleted("react[0].round-1.tool-0"); !ok {
		t.Fatal("tool-0 leaf not committed")
	}
	if len(h.rs.LookupReactRounds("react[0]")) != 1 { // round-1 closed; round-2 natural-stops (no marker needed/maybe 1)
		t.Fatalf("rounds = %d", len(h.rs.LookupReactRounds("react[0]")))
	}
	// the second invocation's messages must include the tool result with matching tool_call_id
	call2 := h.runner.Calls()[1]
	if !hasToolMessage(call2.Messages, "c1", "RESULT-OK") {
		t.Fatalf("round-2 messages missing tool result: %+v", call2.Messages)
	}
}
```

> Decide and pin the round-closing semantics consistent with the spec's resume model: a round that dispatched tools commits its `react.round` marker (so `ReactRounds` counts it); a natural-stop round may or may not emit a marker (the terminal `R` commit is the resume anchor). Pick: emit the marker **only for rounds that dispatched tools and continue** (the final/natural-stop round relies on the terminal `R`). Make the assertion match your choice and pin it in the resume test (4.5).

- [ ] **Step 2: Run — expect FAIL** (stub `dispatchRoundTools` errors).

Run: `go test -race ./engine/ -run TestRunReactDispatchesToolThenAnswers`
Expected: FAIL

- [ ] **Step 3: Implement `dispatchRoundTools`** — for each `tool_call` in `Index` order: guard `LookupCompleted(ToolPath)`; look up the tool by name (unknown → feed an error tool message, no dispatch); stage the **verbatim** arguments bytes via `container.InputFile` at a per-call path; build a synthesized `*ir.CodeStep` from the tool's `ToolImpl` templated through `toolImplScope`; dispatch via the same path `runCommandReduce` uses (`RunWithRetry` → `Commit`); convert a non-zero exit into a fed-back tool result (don't fail the step); append a `tool` `ReactTurn` with the bounded result + matching `ToolCallID`.

```go
func dispatchRoundTools(ctx context.Context, r *ir.React, roundPath string, mr ToolLoopRoundResult, msgs *[]agent.ReactTurn, ictx interpreterContext) error {
	for _, tc := range mr.ToolCalls { // already in Index order
		toolPath := ToolPath(roundPath, tc.Index)
		if nr, ok := ictx.runstate.LookupCompleted(toolPath); ok {
			*msgs = append(*msgs, toolMessageFromLeaf(tc.ID, nr))
			continue
		}
		tool, ok := r.toolByName(ictx.wf, tc.Name) // tc.Name must be in r.Tools
		if !ok {
			*msgs = append(*msgs, agent.ReactTurn{Role: "tool", ToolCallID: tc.ID,
				Content: fmt.Sprintf("error: unknown tool %q", tc.Name)})
			continue
		}
		result, err := dispatchOneTool(ctx, tool, toolPath, tc, ictx) // stages args, synth CodeStep, RunWithRetry, Commit
		if err != nil {
			return err // genuine infra/dispatch failure (after retry) — hard fail
		}
		*msgs = append(*msgs, agent.ReactTurn{Role: "tool", ToolCallID: tc.ID, Content: boundToolResult(result)})
	}
	return nil
}
```

> Implement `dispatchOneTool` by adapting `runCommandReduce` (`reduce.go:215-308`): stage `[]container.InputFile{{Path: argsFilePath(toolPath), Content: []byte(tc.Arguments)}}`; `cmd := template.Substitute(tool.Impl.Run, newToolImplScope(rs, wf, toolPath, argsFilePath(toolPath), parseArgsBestEffort(tc.Arguments)))`; build `NodeIntent`/`ResolvedInputs` with the synthesized `&ir.CodeStep{Run: cmd, Container: tool.Impl.Container, OutputSchema: tool.Impl.OutputSchema, OutputFiles: tool.Impl.OutputFiles, InputFiles: <staged>}`; `RunWithRetry` → on a tool's own non-zero exit, capture `{exit_code, stdout}` and `Commit` an OK leaf with that as the result (the divergence from a normal code step); `rs.RecordCompleted`. `boundToolResult` applies the 16384-byte `boundDisplayField`-style cap + the `utf8.Valid` descriptor route (Task 4.6); for now (4.3) inline a TODO calling a `boundToolResult` that returns stdout unbounded, replaced in 4.6.

> **Implement `closeRound`** (the gate.go:161-170 marker pattern):

```go
func closeRound(ictx interpreterContext, reactPath string, k int) error {
	data, err := json.Marshal(ReactRoundData{N: k})
	if err != nil {
		return fmt.Errorf("engine.runReact: marshal react.round at %q: %w", reactPath, err)
	}
	if err := ictx.log.Append(state.Event{Type: EventReactRound, Path: reactPath, Data: data}); err != nil {
		return fmt.Errorf("engine.runReact: append react.round at %q: %w", reactPath, err)
	}
	if err := ictx.log.Sync(); err != nil { // deliberate Sync (gate-style), unlike loop.iter
		return fmt.Errorf("engine.runReact: sync react.round at %q: %w", reactPath, err)
	}
	ictx.runstate.RecordReactRound(reactPath, ReactRoundRecord{N: k})
	return nil
}
```

> **Marker event Path = `reactPath` (= `R`)**, NOT the round path — matching the gate.attempt/loop.iter precedent so `Fold` keys `ReactRounds[R]` and `LookupReactRounds(R)` matches.

- [ ] **Step 4: Run — expect PASS.**

Run: `go test -race ./engine/ -run TestRunReactDispatchesToolThenAnswers`
Expected: PASS

- [ ] **Step 5: Commit.**

```bash
go test -race ./engine/ && gofmt -l engine/
git add engine/react.go engine/react_test.go
git commit -m "feat(engine): runReact tool dispatch + react.round marker (P3)"
```

### Task 4.4: Replay — rebuild messages from committed leaves; single `buildAssistantTurn`

**Files:**
- Modify: `engine/react.go` (`replayMessages`, `buildAssistantTurn`, `roundResultFromLeaf`)
- Test: `engine/react_test.go` (append)

- [ ] **Step 1: Write the failing test** — fold a pre-seeded log with a committed round-1 (model leaf + tool-0 leaf + marker), then call `runReact`; assert round 1 is NOT re-sampled (the fake runner's first script entry is consumed by round 2, not round 1) and the rebuilt messages contain round 1's assistant-with-tool_calls + tool result with matching IDs.

```go
func TestRunReactReplaysCommittedRound(t *testing.T) {
	h := newReactTestHarness(t, /* react with tool check */, scriptedToolLoop{
		{Text: `{"answer":"final"}`, FinishReason: "stop"}, // this must be consumed by ROUND 2
	})
	h.seedCommittedRound1(t, /* model: tool_call c1 check {"x":1}; tool-0: "RES" */)
	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: %v %v", oc, err)
	}
	if got := len(h.runner.Calls()); got != 1 {
		t.Fatalf("model called %d times, want 1 (round 1 replayed, not re-sampled)", got)
	}
	// the single (round-2) invocation's history must include round-1 assistant(tool_calls)+tool(result)
	msgs := h.runner.Calls()[0].Messages
	assertAssistantToolCall(t, msgs, "c1", "check", `{"x":1}`)
	assertToolMessage(t, msgs, "c1", "RES")
}
```

- [ ] **Step 2: Run — expect FAIL** (`replayMessages` returns only the initial prompt; the model is called for round 1 → 2 calls, or IDs mismatch).

Run: `go test -race ./engine/ -run TestRunReactReplaysCommittedRound`
Expected: FAIL

- [ ] **Step 3: Implement `replayMessages`** — for `k := 1; k < startK; k++`: read `LookupCompleted(ModelPath(RoundPath(reactPath,k)))` → `buildAssistantTurn` (from the stored `tool_calls`/text — the SAME function the fresh path uses); for each `tool_call`, read `LookupCompleted(ToolPath(...))` → a `tool` `ReactTurn` with the matching `ToolCallID`. Prepend the initial `user: r.Prompt`. **`buildAssistantTurn` must omit `tool_calls` entirely when none** (empty `[]` is a 400).

```go
func buildAssistantTurn(mr ToolLoopRoundResult) agent.ReactTurn {
	turn := agent.ReactTurn{Role: "assistant", Content: mr.Text}
	if len(mr.ToolCalls) > 0 { // OMIT when none — empty tool_calls is an OpenAI 400
		turn.ToolCalls = mr.ToolCalls
	}
	return turn
}
```

> `roundResultFromLeaf(nr)` reconstructs a `ToolLoopRoundResult` from `nr.Outputs` (the committed `{text, finish_reason, tool_calls}` map). The verbatim-args invariant rides here: `tool_calls[i].arguments` is the Go string stored on the leaf, round-tripped byte-identically.

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./engine/ && gofmt -l engine/
git add engine/react.go engine/react_test.go
git commit -m "feat(engine): runReact replay from committed leaves; single buildAssistantTurn (P3)"
```

### Task 4.5: `max_turns` truncation contract

**Files:**
- Modify: `engine/react.go` (`commitTerminal` for `max_turns`)
- Test: `engine/react_test.go` (append)

- [ ] **Step 1: Write the failing test** — `max_turns: 1`, the model keeps requesting tools. Assert: the round-1 tools are NOT dispatched (no `tool-0` leaf), the terminal output is `{stop_reason:"max_turns", text:...}`, `output_schema` is NOT enforced, outcome is OK.

```go
func TestRunReactMaxTurnsTruncates(t *testing.T) {
	h := newReactTestHarness(t, /* react max_turns:1, output_schema {answer required} */, scriptedToolLoop{
		{Text: "partial", ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: "{}"}}, FinishReason: "tool_calls"},
	})
	oc, err := h.run(t)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("run: %v %v", oc, err)
	}
	if _, ok := h.rs.LookupCompleted("react[0].round-1.tool-0"); ok {
		t.Fatal("max_turns must NOT dispatch the dangling round's tools")
	}
	nr, _ := h.rs.LookupCompleted("react[0]")
	if nr.Outputs["stop_reason"] != "max_turns" || nr.Outputs["text"] != "partial" {
		t.Fatalf("terminal = %v", nr.Outputs)
	}
	if _, hasAnswer := nr.Outputs["answer"]; hasAnswer {
		t.Fatal("output_schema must NOT be enforced on max_turns truncation")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (current `commitTerminal` may try to validate the schema or dispatch tools).

Run: `go test -race ./engine/ -run TestRunReactMaxTurnsTruncates`
Expected: FAIL

- [ ] **Step 3: Implement** the two `commitTerminal` cases (the loop already decides before tool dispatch — Task 4.2 step 3):
  - natural stop: parse the final assistant `Text` against `output_schema` if declared (reuse the layer-2 typed-output validation); output = validated object + `stop_reason:"stop"`.
  - max_turns: output = `{text: mr.Text, stop_reason:"max_turns"}`; do NOT validate `output_schema`.

```go
func commitTerminal(ictx interpreterContext, r *ir.React, reactPath string, mr ToolLoopRoundResult, stopReason string) (Outcome, error) {
	out := map[string]any{}
	if stopReason == "max_turns" {
		out["text"] = mr.Text
	} else {
		if r.OutputSchema != nil {
			obj, err := parseTypedOutput(mr.Text, r.OutputSchema) // existing layer-2 validation
			if err != nil {
				return failStep(ictx.log, reactPath, OutcomeRetryableFailure, err)
			}
			for k, v := range obj {
				out[k] = v
			}
		} else {
			out["text"] = mr.Text
		}
	}
	out["stop_reason"] = stopReason
	nr, err := Commit(ictx.log, ictx.blobs, reactPath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)
	if err != nil {
		return "", fmt.Errorf("engine.runReact: commit terminal at %q: %w", reactPath, err)
	}
	ictx.runstate.RecordCompleted(reactPath, nr)
	return OutcomeOK, nil
}
```

> Use the real name of the layer-2 typed-output parser (find how `agent` steps validate `output_schema` for `awf/llm`, `NativeSchema:false` — `local_dispatcher_agent.go:236-252`).

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./engine/ && gofmt -l engine/
git add engine/react.go engine/react_test.go
git commit -m "feat(engine): runReact max_turns truncation + natural-stop schema (P3)"
```

### Task 4.6: Bound the model-facing tool result + non-UTF-8 descriptor

**Files:**
- Modify: `engine/react.go` (`boundToolResult`)
- Test: `engine/react_test.go` (append)

- [ ] **Step 1: Write the failing test.**

```go
func TestBoundToolResult(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 20000)
	out := boundToolResult(container.ExecResult{ExitCode: 0, Stdout: big})
	if len(out) > 17000 { // 16384 + marker headroom
		t.Fatalf("not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("missing truncation marker")
	}
	// non-UTF-8 → descriptor, not raw bytes
	bin := boundToolResult(container.ExecResult{ExitCode: 0, Stdout: []byte{0xff, 0xfe, 0x00}})
	if strings.ContainsRune(bin, '�') || strings.Contains(bin, "\x00") {
		t.Fatal("binary output must be a descriptor, not inlined bytes")
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./engine/ -run TestBoundToolResult`
Expected: FAIL

- [ ] **Step 3: Implement** `boundToolResult` (the `boundDisplayField` byte-cap pattern, `agent_step.go:383`; cap 16384; `utf8.Valid` gate). The full output is already in the committed `.tool-J` leaf — this only bounds what the model sees.

```go
const toolResultCap = 16384

func boundToolResult(res container.ExecResult) string {
	body := res.Stdout
	if !utf8.Valid(body) {
		return fmt.Sprintf("[non-text tool output: %d bytes, exit %d — see the run's artifacts]", len(body), res.ExitCode)
	}
	s := string(body)
	if len(s) > toolResultCap {
		// head+tail with a UTF-8 rune-boundary backup (boundDisplayField pattern)
		head := safeTrunc(s, toolResultCap)
		s = fmt.Sprintf("%s\n…[truncated %d bytes — full output in artifacts]", head, len(s)-len(head))
	}
	if res.ExitCode != 0 {
		s = fmt.Sprintf("[exit %d]\n%s", res.ExitCode, s)
	}
	return s
}
```

> Implement `safeTrunc` to back up to a rune boundary (copy the `boundDisplayField` body at `engine/agent_step.go:383`). Wire `dispatchOneTool` (Task 4.3) to use `boundToolResult` on the captured `ExecResult` for the model-facing message, while the committed `.tool-J` leaf keeps the full output.

- [ ] **Step 4: Run — expect PASS, full engine suite, lint+vet, commit.**

```bash
go test -race ./engine/ && gofmt -l engine/ && go vet ./engine/
git add engine/react.go engine/react_test.go
git commit -m "feat(engine): bound model-facing tool result + non-UTF-8 descriptor (P3)"
```

### Task 4.7: Wire `runReact` into the run loop (dependencies: resolver, broker)

**Files:**
- Modify: `engine/interpreter.go` / wherever `interpreterContext` is built (ensure `resolver` is available to `runReactWithContext`)
- Test: an `engine` integration-style test or rely on the conformance bucket (Phase 5)

- [ ] **Step 1: Confirm `interpreterContext` carries what `runReact` needs** — the agent `resolver` (it does for agent steps; reuse the same field). If `runReactWithContext` references a field not on `interpreterContext`, add it where the context is constructed (mirror how `runAgentStepWithContext` gets the resolver). Build and run the full engine suite.

Run: `go build ./... && go test -race ./engine/`
Expected: PASS

- [ ] **Step 2: Commit** (if any wiring changed).

```bash
git add engine/interpreter.go
git commit -m "chore(engine): wire resolver into runReact context (P3)"
```

---

# Phase 5 — Conformance: fake tool-loop adapter + run/resume buckets

### Task 5.1: Fake `ToolLoopRunner` (scripted tool_calls)

**Files:**
- Modify: `agent/fake/fake.go` (extend the fake to satisfy `agent.ToolLoopRunner`) OR create `agent/fake/toolloop.go`
- Test: `agent/fake/fake_test.go` (append)

- [ ] **Step 1: Write the failing test** — the fake satisfies `ToolLoopRunner`, returns the scripted result per call index, records `Calls()` (the inspection seam for ID-equality assertions), and (the resume tripwire) PANICS if asked to run a call index that was already consumed in a prior lifetime.

```go
func TestFakeToolLoopScriptedAndTripwire(t *testing.T) {
	f := New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		ScriptToolLoop(0, agent.ToolLoopResult{FinishReason: "tool_calls", ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "t"}}}).
		ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Text: "done"})
	var _ agent.ToolLoopRunner = f
	r0, _ := f.RunToolLoop(context.Background(), agent.ToolLoopInvocation{})
	if r0.FinishReason != "tool_calls" {
		t.Fatalf("call 0 = %+v", r0)
	}
	if len(f.Calls()) != 1 {
		t.Fatalf("Calls = %d", len(f.Calls()))
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test -race ./agent/fake/ -run TestFakeToolLoopScriptedAndTripwire`
Expected: FAIL

- [ ] **Step 3: Implement.** Add a `toolLoopScript map[int]agent.ToolLoopResult`, a `toolLoopCalls []agent.ToolLoopInvocation` (returned defensively-copied by `Calls()` or a new `ToolLoopCalls()`), a `ScriptToolLoop(idx, res)` builder, and `RunToolLoop` (mutex-guarded; increments an index; **panics** if the requested index < the highest already-served index in a *resumed* run — i.e. a "tripwire" mode the resume test enables). Keep `Caps` `{Containerless:true, Threaded:true}` so `runReact`'s gate passes.

> The tripwire: in the resume test, construct the fake so it has *no* script for already-committed rounds and `panic`s (not just errors) if `RunToolLoop` is invoked for them — proving the model is never re-sampled. Add a `WithToolLoopTripwire(committedRounds int)` option.

- [ ] **Step 4: Run — expect PASS, commit.**

```bash
go test -race ./agent/fake/ && gofmt -l agent/fake/
git add agent/fake/ && git commit -m "feat(fake): scripted ToolLoopRunner + resume tripwire (P3)"
```

### Task 5.2: `testReact` conformance bucket — happy path

**Files:**
- Create: `conformance/react.go`
- Modify: `conformance/suite.go` (register `t.Run("react", ...)`)
- (test entry is the existing `TestConformanceFakeBackend`)

- [ ] **Step 1: Write `testReact`** with an inline workflow YAML string (the `tools:` block + a `react:` node + a `containers:` entry), wiring the fake `ToolLoopRunner` via `newHarnessWithAgentRegistry` and the fake backend (program the tool impl's exec via `preProgramFake`). Assert run outcome OK + the terminal output + the `react[0].round-1.tool-0` leaf. Register it in `RunSuite`:

```go
// conformance/suite.go, in RunSuite:
	t.Run("react", func(t *testing.T) { testReact(t, factory) })
```

```go
// conformance/react.go
package conformance

const reactToolLoopWorkflow = `
workflow: react-tool-loop
version: 1
containers:
  fin: { image: "oci://busybox@sha256:..." }
tools:
  check:
    description: echo the iban
    input_schema: { type: object, properties: { iban: { type: string } }, required: [iban] }
    impl:
      run: "cat {{ args_file }}"
      container: fin
graph:
  - react:
      id: answer
      with: { uses: awf/llm, model: m }
      prompt: "{{ input.q }}"
      tools: [check]
      max_turns: 4
      output_schema: { type: object, properties: { answer: { type: string } }, required: [answer] }
outputs:
  final: "{{ answer.answer }}"
`

func testReact(t *testing.T, factory BackendFactory) {
	t.Helper()
	var llm *fake.Fake
	register := func(reg *agent.Registry) {
		llm = fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true}).
			ScriptToolLoop(0, agent.ToolLoopResult{FinishReason: "tool_calls",
				ToolCalls: []agent.ToolCall{{Index: 0, ID: "c1", Name: "check", Arguments: `{"iban":"DE89"}`}}}).
			ScriptToolLoop(1, agent.ToolLoopResult{FinishReason: "stop", Text: `{"answer":"validated"}`})
		if err := reg.Register(llm); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	progFactory := preProgramFake(t, factory, []execProgram{{cmd: "cat", res: container.ExecResult{ExitCode: 0, Stdout: []byte(`{"iban":"DE89"}`)}}})
	h := newHarnessWithAgentRegistry(t, progFactory, reactToolLoopWorkflow, register)
	h.input = map[string]any{"q": "validate DE89"}
	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("run: %v %v", oc, err)
	}
	// fold + assert the terminal output + the tool leaf
	rs := mustFold(t, h)
	if rs.Completed["react[0]"].Outputs["answer"] != "validated" {
		t.Fatalf("terminal = %v", rs.Completed["react[0]"].Outputs)
	}
	if _, ok := rs.Completed["react[0].round-1.tool-0"]; !ok {
		t.Fatal("tool-0 leaf missing")
	}
}
```

> Adapt `mustFold`/`rs.Completed` access to the harness's actual fold helper (`mustFoldEvents` + `engine.Fold`). Match the fake-backend exec-programming convention exactly (cmd match semantics).

- [ ] **Step 2: Run — expect FAIL → implement until PASS.**

Run: `go test -tags=integ -count=1 -p 1 ./conformance/ -run TestConformanceFakeBackend/react`
Expected: first FAIL (handler/wiring), then PASS

- [ ] **Step 3: Commit.**

```bash
git add conformance/react.go conformance/suite.go
git commit -m "test(conformance): react: happy-path tool loop bucket (P3)"
```

### Task 5.3: `testReact` resume — rounds replay, model not re-sampled, IDs match

**Files:**
- Modify: `conformance/react.go` (add a resume sub-test)

- [ ] **Step 1: Write the resume sub-test** — run, then resume against a tripwire fake; assert (a) resume succeeds, (b) the model is NOT re-sampled for committed rounds (the tripwire would panic), (c) `assistant.tool_calls[*].id == tool.tool_call_id` on the rebuilt history (inspect `llm.ToolLoopCalls()`), (d) the torn-frontier case (kill after the `.model` leaf commits but before `tool-0`) re-runs only the uncommitted tool. Use the existing crash-fault-injection patterns (`conformance/atomic.go` / `continues_crash.go`) to stop after a chosen event.

```go
func testReactResume(t *testing.T, factory BackendFactory) {
	// 1. First lifetime: fault-inject a stop after react[0].round-1's marker.
	// 2. Second lifetime: resume against a fake whose round-1 script is a TRIPWIRE
	//    (panics if RunToolLoop is called for round 1) and whose round-2 script
	//    returns the final answer.
	// 3. Assert: resume OK; model called exactly once (round 2); the round-2
	//    invocation's messages replay round-1 assistant(tool_calls c1)+tool(c1) with
	//    matching IDs.
}
```

> Implement using the harness's run-then-resume flow (`h.runWorkflow` with a fault hook, then `h.resumeWorkflow`). The fault-hook + tripwire wiring follows `conformance/continues_crash.go`. Add a sub-test for two-calls-to-the-same-tool-in-one-round → distinct `tool-0`/`tool-1`.

- [ ] **Step 2: Run — implement until PASS.**

Run: `go test -tags=integ -count=1 -p 1 ./conformance/ -run TestConformanceFakeBackend/react`
Expected: PASS

- [ ] **Step 3: Commit.**

```bash
git add conformance/react.go
git commit -m "test(conformance): react: resume replay + tripwire + ID-equality (P3)"
```

### Task 5.4: `react:` via an `agents:` role (DerivedAdapter forwarding) conformance

**Files:**
- Modify: `conformance/react.go` (add a sub-test) OR `agent/derived_test.go` (a focused unit test if the conformance role wiring is heavy)

- [ ] **Step 1: Write the test** — a workflow whose `react.with.uses` names an `agents:` role bound to the fake awf/llm; assert the tool loop runs (proving the `DerivedAdapter` `RunToolLoop` forwarding + interface assertion path, not concrete-type erasure). If the conformance role-registration is heavy, a unit test in `agent/` asserting `(*DerivedAdapter)` satisfies `ToolLoopRunner` and forwards (already in Task 3.2) plus an `engine` test where `toolLoopRunnerFor` resolves through a `DerivedAdapter` covers C2.

- [ ] **Step 2: Run — PASS. Step 3: Commit.**

```bash
git add conformance/react.go && git commit -m "test(conformance): react: via agents: role (DerivedAdapter forwarding) (P3)"
```

### Task 5.5: Full green bar

- [ ] **Step 1: Run the whole bar.**

Run:
```bash
make lint test
go test -tags=integ -count=1 -p 1 ./conformance/...
```
Expected: all PASS (lint clean: gofmt, go vet, golangci-lint).

- [ ] **Step 2: Fix any lint/vet findings; re-run; commit any fixes.**

```bash
git add -A && git commit -m "chore(P3): lint/vet cleanups; full green bar"
```

---

# Phase 6 — P2 native-resume documentation note (independent)

### Task 6.1: Document the `--backend native` resume boundary

**Files:**
- Modify: `man/awf.1.md` (the `resume` command section)
- Modify: `README.md` (the backends/limitations area)

- [ ] **Step 1: Add the note** to `man/awf.1.md` under `awf resume` (text matching the error at `cli/backend.go:108`):

```markdown
> **Native backend is not resumable.** `awf resume` of a run started with `--backend native`
> errors: there is no infra recipe to reconstruct host state. Two escape hatches: use
> `--backend docker` for resumable runs, or re-drive a deterministic run from the start
> (delete the run directory and `awf run --backend native …` again).
```

- [ ] **Step 2: Add a one-line mirror** to `README.md` where backends are described.

- [ ] **Step 3: Verify** the wording matches `cli/backend.go:108`'s error message intent; no code changes.

- [ ] **Step 4: Commit.**

```bash
git add man/awf.1.md README.md
git commit -m "docs: document --backend native non-resumable boundary (P2)"
```

---

## Self-review (run before handing off)

**Spec coverage:** A4 `tools:` (Tasks 1.1–1.2, 4.3) ✓ · `react:` node + 4-edit registry (1.3) ✓ · validation incl. gating/Ollama/reserved/producer/args-carveout (1.4–1.6) ✓ · journaling path/event/runstate/fold (2.1–2.4) ✓ · `ToolLoopRunner` + `DerivedAdapter` forwarding + transport + prompt-exempt validate (3.1–3.5) ✓ · `runReact` natural/tool/replay/max_turns/bound (4.2–4.6) ✓ · conformance happy/resume/role + green bar (5.x) ✓ · P2 doc (6.1) ✓. Spec §4.4 crash≠verdict is exercised by the torn-frontier resume sub-test (5.3).

**Placeholder scan:** the code blocks are concrete; the `> Note:` callouts flag where an exact existing symbol name must be confirmed against the worktree before pasting (e.g. `JSONSchema`/`OutputFiles` type names, the layer-2 typed-output parser, `reqConfigFrom`). These are verification gates, not placeholders — resolve each by grep before writing the code in that step. **Replace the placeholder AWF codes (AWF1050–AWF1056) with the actual next-free codes** found in `ir/`.

**Type consistency:** `ir.Tool`/`ir.ToolImpl` (1.1) used in 1.4/4.3; `ir.React` fields (1.3) used in 1.4/4.2; `EventReactRound`/`ReactRoundData` (2.2) used in 2.4/4.3; `ReactRounds`/`ReactRoundRecord`/`Lookup/RecordReactRounds` (2.3) used in 2.4/4.2; `RoundPath/ModelPath/ToolPath` (2.1) used in 4.x; `agent.ToolLoopRunner`/`ToolLoopInvocation`/`ToolLoopResult`/`ReactTurn`/`ToolCall`/`ToolDef` (3.1) used in 3.2/3.4/3.5/4.x/5.x — names are consistent across phases.

**Known open items deferred to implementation (pinned, not vague):** the exact internal `ToolLoopRoundResult` helper struct shape in `engine/react.go` (a private mirror of `agent.ToolLoopResult`); the `safeTrunc`/`metricsFrom`/`reqConfigFrom` helper names (confirm against existing code). None affect the public format or the journaling contract.
