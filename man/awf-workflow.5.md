AWF-WORKFLOW 5 "May 2026" "AWF" "AWF Manual"
============================================

# NAME

awf-workflow - the Agentic Workflow Format (AWF)

# DESCRIPTION

The Agentic Workflow Format (AWF) is a declarative format for *agentic pipelines*:
author-defined control flow whose steps are black-box agent CLIs (such as Claude
Code), shell commands, and external-signal waits, run against long-lived
containers, with an independent judge gating each stage. A workflow is a single
YAML document executed by **awf**(1). The current format version is **1**.

Its central construct is the **gate** (see **CONTROL FLOW AND THE GATE**): a
generator, an independent evaluator, and a bounded repair loop. The evaluator is
either an LLM judge or a deterministic check, and it runs independently of the
generator, so the agent's self-report is never the verdict.

This page is the format reference — the fields and their meaning. For what AWF is
and why it works this way, see the project README. The whole document is
content-addressed when a run starts; resuming a run requires the identical
definition (see **CHECKPOINTING AND RESUME**).

# TOP LEVEL

A workflow document has the following top-level shape:

    workflow: <id>
    version: 1
    input: <json-schema>          # optional; run parameters
    containers:
      <name>: { image: <oci-ref>, resources: { cpu, mem } }   # or compose (see CONTAINERS)
    graph: [ <node>, ... ]

**workflow**
:   Required. A stable identifier for the workflow.

**version**
:   Required. The AWF format version. Current value: `1`.

**input**
:   Optional. A JSON Schema (see **TEMPLATING AND TYPED OUTPUTS**) for the run
    parameters, referenced as `{{ input.<field> }}`.

**containers**
:   Required. The infrastructure the workflow runs against (see **CONTAINERS**).

**graph**
:   Required. An ordered list of nodes. Sequential composition is implicit:
    sibling nodes run in order.

# CONTAINERS

A declared container is a long-lived instance, created on first use and shared by
every step that names it, for the life of the run. This fits agentic work: an
agent operates in a workspace — it writes files one step reads the next — and a
lab (a database, a browser, a staging API) must stay up
across the generate/evaluate/repair cycle.

A container is backed by either a single digest-pinned image or a Compose project
— never both:

    containers:
      lab:                                  # a multi-service lab
        compose: ./lab/compose.yml          # every image inside is digest-pinned
        service: web                        # the service steps exec into by default
      scratch:                              # a single image
        image: oci://registry.example.com/runner@sha256:abc...   # a digest, not a tag
        resources: { cpu: 2, mem: 4Gi }

**image**
:   One of `image`/`compose`. A single OCI image, content-addressed by digest. A
    mutable tag is rejected, because it would break resume.

**compose**
:   One of `image`/`compose`. A Compose file for a multi-service lab. Every
    `image:` inside it must be digest-pinned (the validator checks each `image:`
    is `@sha256:`-pinned; it does **not** verify that a service which `build:`s
    its image locally actually matches that digest, so a `build:`+`image:` service
    silently defeats reproducibility), and the file's bytes fold into the workflow
    definition digest.

**service**
:   Required with `compose`. The service that `run:`/`uses:` exec into by
    default. `container: lab:db` addresses another service in the same project.

**resources.cpu** / **resources.mem**
:   Used with `image`. vCPU and memory for a single-image container. For a
    Compose project, resources live per-service in the Compose file.

Compose is Docker's job, not AWF's. Networks, `depends_on`, `healthcheck`, and
multi-service wiring are expressed in the Compose file using Docker's own
machinery. AWF only validates digest-pinning, brings the project up run-scoped
(`up --wait`), routes exec to a service, and tears the project down at run end.

Readiness is re-established on every (re)creation, including resume. The runtime
guarantees a container is healthy before dispatching a step into it; it does not
define its own readiness mechanism. A single image becomes ready via its
entrypoint/CMD; a Compose project via healthchecks and `up --wait`. There is
deliberately no `setup` step and no per-step "re-run on resume" flag.

State lives in three places, handled three ways:

**Durable outputs**
:   A step's typed outputs and any declared `output_files` commit to a
    content-addressed artifact store. This is what survives a crash and what
    resume reads back.

**Infrastructure**
:   Reconstructed from the image/Compose recipe on every (re)creation. A rebuilt
    lab is *more* reproducible than a restored image — the property the digest
    pin already demands.

**Unmanaged in-container mutation**
:   In-container process or filesystem mutation that is neither a declared output
    nor reconstructable from the recipe is *not* preserved across a checkpoint.

For the one case the recipe cannot serve — an agent that mutated a workspace
expensively and nondeterministically, where a later step needs that state after
resume (a coding agent's evolving working tree) — a container can opt in to a
filesystem snapshot:

    workspace:
      image: oci://...@sha256:...
      snapshot: workspace        # capture a copy-on-write FS diff at each commit; restore on resume

The runtime then captures a copy-on-write diff (not a squashed commit) at each
commit boundary and restores it instead of rebuilding from the recipe. It is off
by default and scoped to mutable-workspace containers.

Two consequences to keep in mind:

- Parallel branches that mutate state need distinct containers / Compose
  projects; the validator rejects concurrent writers to one workspace.
- Loop and repair iterations accumulate state in the same container — usually
  what you want (the lab stays up), occasionally not (reset explicitly with a
  step).

# STEPS

A step node is exactly one of three black boxes; AWF runs it and does not look
inside. A node with more than one, or none, is invalid.

- Code step (`run:`) — a shell command.
- Agent step (`uses:`) — an external agent-runtime invocation.
- Signal step (`await:`) — block until an external signal.

## Code step (run)

    - id: <id>
      container: <name>
      run: <command>
      timeout: <dur>                 # optional; on expiry -> retryable_failure
      output_schema: { ... }         # optional; step writes JSON to $AWF_OUTPUT
      output_files: [<path>, ...]    # optional; captured into the artifact store on commit
      idempotency_key: <template>    # optional; for effects outside the container
      retry: { ... }                 # optional

Implicit outputs are always `exit_code` and `stdout`. `output_schema` adds typed
fields (the step writes conforming JSON to the file named by `$AWF_OUTPUT`; the
runtime sets that variable but does **not** create its parent directory, so the
step must `mkdir -p "$(dirname "$AWF_OUTPUT")"` before writing — a missing file
is a `retryable_failure`, not a typed verdict);
`output_files` captures named artifacts. A nonzero exit is a `retryable_failure`
unless its code is declared permanent (see **OUTCOMES, RETRY, AND REPAIR**).

## Agent step (uses)

Delegates a task to a named agent runtime and captures a typed result. AWF
carries an opaque `with:` map whose schema the runtime owns and validates, so the
format never hard-codes one harness's options.

    - id: <id>
      container: <name>
      uses: <agent-runtime-ref>      # e.g. anthropic/claude-code, factory/droid, or block/goose
      with: { ... }                  # opaque; validated by the runtime
      output_schema: { ... }         # required iff outputs are referenced downstream
      output_files: [<path>, ...]    # optional
      timeout: <dur>                 # optional
      idempotency_key: <template>    # optional
      retry: { ... }                 # optional

**uses**
:   Required. The runtime ref. Resolution is runtime-defined; the identity *and
    version* are pinned at run start.

**with**
:   Required. Opaque runtime config (one runtime takes `{model, prompt, tools,
    max_turns}`; another takes `{models, budget}`). The core never reads its
    keys.

**output_schema**
:   Required iff a `step.<id>.<field>` of this step is referenced elsewhere.

Outputs are typed, never free text — this is what makes the judge work. When
`output_schema` is declared the runtime produces conforming output, via its
constrained/structured-output mode or schema-aligned parsing of the final message
(tolerant of fences, prose, and minor slips). If neither yields a conforming
value within the retry budget the step is a `retryable_failure`. References bind
only to typed fields, so `**verdict: pass**` versus `verdict: pass` can never
silently break a gate.

Agent steps are atomic: one invocation is one checkpoint boundary, and resume
re-runs the whole step from its pre-step snapshot. The agent's internal loop is
its own business.

## Signal step (await)

    - id: approve
      await: <signal-name>
      timeout: <dur>                 # optional; on expiry -> retryable_failure (no payload)
      output_schema: { ... }         # optional; validates the delivered payload

Blocks until a signal of that name is delivered (for example, a human approval
before opening a PR). Signals are durable and buffered: journaled on receipt even
before the `await` is reached, consumed earliest-first per name, and never lost
across a restart. No container is needed. Deliver one with **awf signal** (see
**awf**(1)).

# CONTROL FLOW AND THE GATE

Control flow is author-defined and block-structured — composed by nesting, not by
declaring DAG edges. Data flows implicitly through the shared container and
explicitly through typed references (see **TEMPLATING AND TYPED OUTPUTS**).

## if

    - if: { cond: <expr>, then: [<node>...], else: [<node>...] }   # else optional

Branches on a typed condition. A false `cond` with no `else` is a no-op. Combined
with `skip`, this routes a stage out of a pipeline without nesting everything
after it.

## loop

    - loop: { until: <expr>, max_iters: <n>, body: [<node>...] }   # at least one of until/max_iters

`body` repeats; `until` is tested *after* each iteration (do-while), so it may
read what the body just produced. A reference to a step inside the loop resolves
to its most recent iteration. Use a `loop` for plain repetition with no judge
(polling, or a fixed worklist). For generate-and-judge, use a `gate`, not a bare
loop; for a data-driven worklist whose size is known only at runtime, use `map`.

## try

    - try: { do: [<node>...], catch: [<node>...], finally: [<node>...] }   # catch/finally optional

On a failure escaping `do`, runs `catch`. `finally` runs unconditionally —
including on cancellation — for app-level cleanup (close handles, post a status,
revoke a token). Container/Compose teardown is automatic at run end and on
cancellation, so `finally` is *not* needed for that. AWF has no separate
compensation primitive.

## parallel

    - parallel: [<node>, ...]

Children run concurrently; the node completes when all do. A child failing after
its retries cancels its siblings, then propagates. Branches that run steps must
target distinct containers / Compose projects — the validator enforces this.

## gate

The flagship — TDD applied to a black-box step. A gate runs a *generator*, then an
*independent evaluator* (the test), and if the evaluator's bar is not met it
*repairs* — re-running the generator conditioned on the evaluator's feedback —
until the bar is met or attempts run out.

    - gate:
        generate: [<node>, ...]      # produces (and, on repair, revises) the artifact
        evaluate: [<node>, ...]      # the independent judge; verdict = the block's final typed output
        until: <expr>                # pass condition over the verdict
        max_attempts: <n>            # bound on generate->evaluate cycles

Semantics:

1. Run `generate`, then `evaluate`. The *verdict* is the typed output of
   `evaluate`'s last node. Test `until` over it.
2. `until` true — the gate *passes* and flow continues.
3. `until` false — *repair*: re-run `generate`, then `evaluate`, and re-test.
4. Attempts exhausted — the gate is `rejected`, which propagates like any failure
   to the nearest `try`/`catch` (or halts the run).

A verdict is not a crash. The gate distinguishes a *mechanical failure* of
`generate`/`evaluate` (crash, `timeout`, transport — handled by that step's own
`retry`) from a *verdict* (the evaluator ran and `until` is false). Only a verdict
repairs and consumes an attempt. If a step fails mechanically after its own
retries, the gate fails and propagates — you cannot repair a crash, and a broken
judge must never be read as a rejection. So `max_attempts` bounds quality cycles,
never flakiness.

A gate is not a `loop` with a check inside it; the runtime enforces the two
properties that make the pattern work:

**Enforced independence**
:   An LLM judge runs as a *fresh agent context* — a new session, never the
    generator's continued conversation — so it cannot be steered by the
    generator's reasoning; a deterministic/code judge is independent by
    construction. The judge does share the container filesystem (it must, to see
    the artifact), so a good check tests *behavior, not artifacts*: it runs the
    work and confirms the real effect — execute the tests and read the exit code,
    or query the database and count the rows — rather than trusting a status the
    generator wrote.

    The same caution applies to typed outputs: an evaluator may reference a
    generator step's output (commonly a path — `{{ step.<gen>.<path_field> }}` — to
    locate the artifact it must inspect), but it should use that to *test the
    artifact's behavior*, not to trust a self-reported status field the generator
    declared about its own work.

**Automatic feedback**
:   On every attempt after the first, the runtime makes the previous verdict
    available to `generate` — resolvable as `{{ evaluate.<field> }}` and injected
    into an agent generator's context — so regeneration is conditioned on the
    critique. The author does not wire this up. On the first attempt
    `evaluate.*` is empty. Because the verdict is fed into the next generator,
    an evaluator that inspects untrusted or adversarial input must keep raw
    input bytes out of the verdict's typed fields (route them through
    `output_files` instead) — otherwise the verdict becomes an injection channel
    into the generator.

Constraints: `generate` must be non-empty (a gate with no generator cannot
repair); the final node of `evaluate` must declare `output_schema` (the verdict
`until` reads); `max_attempts` is required (stochastic generators can loop
forever). A gate nests anywhere a node can appear, including inside another gate's
`generate`.

Human escalation is a pattern, not a primitive: wrap the gate in `try`/`catch`
with an `await` in the `catch` to put a human in the loop after the repair budget
is spent.

## skip

    - skip: <reason>          # optional reason, recorded in the trace

Cleanly terminates the *nearest enclosing scope* — the current `loop`/`gate`
iteration, a `parallel` branch, or (if none) the *run* — as `ok`, after running
any `finally` blocks it unwinds through. Inside a `parallel` branch it ends only
*that branch* (siblings keep running). It is how a stage bails without nesting the
remainder: "triage found no source -> `skip`; move on." `skip` is `continue`-like,
not `break`: it ends the current iteration/branch, not a whole loop.

## map

    - map:
        over: <expr>                 # a typed array, size known only at runtime
        as: <name>                   # each element bound as {{ <name>.<...> }} and {{ <name>.index }}
        container: <name>            # per-item container/compose instance (one per element)
        concurrency: <n>             # max elements in flight at once
        min_success: <ratio|n>       # optional; fan-in succeeds if at least this many do (default: all)
        body: [<node>...]

Data-driven expansion when the worklist size is known only at runtime — a crawl
finds N pages, a query returns N records. Each element runs `body` in its *own*
container instance (the distinct-container rule applied per element), up to
`concurrency` at a time. `min_success` lets the fan-in tolerate partial failure
instead of cancelling every sibling on the first one. Use `parallel` for a
static, author-known set of distinct branches; use `map` for a runtime-sized set
of identical ones.

A later step reads a `map`'s per-item results in aggregate with a `step.<id>`
reference to a step inside the body, evaluated from outside the map: it lifts that
step's typed output to an index-ordered array (see TEMPLATING AND TYPED OUTPUTS).
That array is legal only as a second `map`'s `over:`, which gives the map→map
chaining shown in EXAMPLE.

# OUTCOMES, RETRY, AND REPAIR

Step outcomes are *mechanical only* — quality is the gate's job, not an outcome
class. Every step ends as exactly one of:

**ok**
:   Ran cleanly (exit 0 / schema-valid / signal delivered). Not retryable.

**retryable_failure**
:   Transient: launch or transport error, `timeout`, a nonzero exit not declared
    permanent, or unparseable output. Retryable per policy.

**permanent_failure**
:   An agent refusal or policy block, or an exit code in
    `non_retryable_exit_codes`. Not retryable.

Retry — transient recovery, applied to every step by default:

    retry: { attempts: 3, backoff: exp, initial: 1s, max: 60s, non_retryable_exit_codes: [78] }

Repair — quality recovery — is the gate, a separate axis. A step can be retried
for flakiness *and* sit inside a gate that repairs it for quality; the two
compose. Retry re-runs an *identical* step after a transient fault, with no
feedback; repair regenerates against the judge's critique.

Propagation: a step that exhausts retries as a failure, or a gate that exhausts
attempts as `rejected`, raises a typed error to the nearest enclosing
`try`/`catch` (a `catch` may match the kind), cancelling parallel siblings on the
way; uncaught, it halts the run.

# TEMPLATING AND TYPED OUTPUTS

Templating does exactly two things; it is not a programming language.

Substitution fills references before a command runs:

    {{ run.id }}   {{ input.<field> }}
    {{ step.<id>.exit_code }}   {{ step.<id>.stdout }}   {{ step.<id>.<field> }}
    {{ evaluate.<field> }}      # inside a gate's generate: the latest verdict; empty on attempt 1

`exit_code` and `stdout` are strings; `<field>` references resolve to *typed
values* from the producer's `output_schema`, never raw text. `evaluate.<field>`
is the typed output of the enclosing gate's `evaluate` block, supplied
automatically on repair attempts. Values over the runtime's inline limit are
rejected at resolution (pass large data as an `output_files` artifact).

`{{` is reserved in every templated field. To write a literal `{{` — a prompt
that teaches templating, or text that merely contains the sequence — escape it
as `\{{`: the backslash is consumed and a literal `{{` is emitted, and the
region is **not** parsed as a reference. This is the only escape; `\` is special
only immediately before `{{` and is otherwise literal (so `\\{{` yields a literal
`\` followed by a literal `{{`). An unescaped `{{` always begins a reference and
must close with `}}`.

A `step.<id>.<field>` reference resolves the named step's typed output wherever
the step sits, subject to scope. `try` and `parallel` introduce no multiplicity,
so a step inside them is referenceable from anywhere, exactly like a top-level
step. A step inside a `loop` resolves to its most recent iteration (above). A
step inside a `gate` or a `map` is referenceable *only from within the same scope
instance* — the same gate attempt, or the same map item — because from outside
there is no single attempt or item to resolve to; a cross-scope reference is
rejected at validation. Read a gate's product through `{{ evaluate.<field> }}`.

A `step.<id>` reference to a step inside a `map` body, evaluated from *outside*
that map, reads the step's per-item outputs in aggregate. It lifts the typed
output to an array, in item-index order:

- `step.<id>` resolves to the array of that step's whole typed outputs — one
  element per item, each element the full `output_schema` object.
- `step.<id>.<field>` resolves to the array of just that `<field>`.

The array is **compact**: it holds one element only for items where the step
actually committed. Items the step never ran for — `if`-filtered, `skip`ped, or
failed before it ran — are simply absent, so the array's length is at most the
worklist size *N* (and an empty `map`, `over: []`, aggregates to `[]`). There are
no nulls; to carry the original-item position through a compacted aggregate, have
the step write it into its own typed output under a field name *other* than
`index` (in a map body, `{{ <as>.index }}` is the reserved item-position
accessor, so an output field literally named `index` cannot be read back).

Because substitution renders only scalars, an aggregate array cannot fill a `{{ }}`
slot in a shell host, a prompt, or a condition — that is rejected at validation
(**AWF5004**). Its one legal use is another `map`'s `over:`, the array-native sink:
map A produces N typed outputs, map B fans out over them. This is the map→map
chaining primitive (see EXAMPLE).

Aggregation in v1 is defined only for the single-map case: the producing step is
enclosed by exactly one `map`, with no `gate` between them and no `loop`
multiplying the path. A producer nested in two or more maps, or wrapped in a
gate, is still rejected as not-yet-defined (**AWF5002**).

Substitution into a shell host (`run:`, `idempotency_key:`) is verbatim and
pre-shell: AWF inserts the resolved value as-is and does **not** quote or escape
it. Use those slots for trusted scalars — ids, counts, enums, flags, `input`
fields. Do not interpolate free-text *agent* output into a shell host; backticks
or `$(...)` in agent-written text are then executed by the shell. Route free-text
or untrusted data through an `output_files` artifact and read it from a file
inside the command. (Composites are rejected mechanically; a free-text `string`
passes both validation and resolution, so keeping it out of shell hosts is the
author's contract. Agent `with:` prompts are not shell hosts.)

Condition evaluation, for `if.cond`, `loop.until`, and `gate.until`, is a bounded
evaluator over references, literals, comparisons (`== != < <= > >=`), and boolean
operators (`&& || !`). No arithmetic, calls, or loops.

Schemas (`input` and every `output_schema`) are JSON Schema 2020-12. For agent
outputs AWF defines a deliberately conservative cross-backend floor: objects with
all properties `required` and `additionalProperties: false`, scalar types,
`enum`, arrays, and bounded nesting; no `oneOf`, `not`, or numeric/string-length
range keywords (`minimum`/`maximum`/`minLength`/`pattern`/...), which no major
constrained-decoding backend enforces. The all-properties-`required` and
`additionalProperties: false` rules are required. Schemas outside the floor are
validated post-hoc, not constraint-enforced.

# CHECKPOINTING AND RESUME

AWF persists progress so a re-run does not redo expensive stages — *not* to
provide distributed exactly-once durability. The durable unit is a
content-addressed artifact, never a live container's process state.

**Commit**
:   The only way a step is recorded complete: its typed outputs and any declared
    `output_files` are written to the artifact store, then a journal entry
    pointing at them is appended (content-address-then-pointer-swap, so a "done"
    record never references a missing artifact). For a `snapshot: workspace`
    container, a copy-on-write FS diff is captured in the same commit.

**Resume**
:   Folds the journal, then: recreates each live container from its image/Compose
    recipe (readiness re-runs via the entrypoint or `up --wait`; a
    `snapshot: workspace` container restores its last committed diff instead);
    *replays committed steps from the journal* — recorded outputs and
    `output_files` are reused, not recomputed; and re-executes only the
    *uncommitted frontier* — the in-flight step on each active branch. A
    deterministic (code) replay is exact; an interrupted agent step may differ on
    re-run, which is correct — its work was never committed.

**Pinning**
:   The workflow definition (by digest, including any Compose files) and each
    resolved agent-runtime identity and version are recorded at run start. Resume
    against a changed definition or runtime is a hard error: a changed definition
    shifts step addressing; a changed runtime changes behavior.

**Pause**
:   Halts dispatch at the next commit boundary, marks the run `paused`
    (non-terminal, resumable), and — unlike cancellation — leaves containers up
    for inspection. This is the breakpoint mechanism; there is no breakpoint node.

**Cancellation**
:   Interrupts in-flight steps, runs enclosing `finally` blocks, tears down
    containers/projects, and marks the run terminal (not resumable).

Step addressing, used for resume and traces, names step nodes by `id` and control
nodes positionally, joined from the root: `try[0].catch`, `if[1].then`,
`loop[0].body.iter-3`, `gate[0].attempt-2.generate`, `parallel[2]`,
`map[0].item-3`.

# EXTERNAL EFFECTS AND IDEMPOTENCY

A step with effects *outside* its container (open a PR, send mail, charge a card)
can be re-run by retry or resume. The mechanism is `idempotency_key`: a stable
template the external system uses to dedupe; the runtime passes the resolved key
to the step on every attempt. Cleanup is `try`/`finally`; there is no
compensation primitive.

AWF can only mediate effects it can see. Effects an agent performs *autonomously
through its own tools* (an `mcp://` call, a network `exec`) are at-least-once and
outside the guarantee. For exactly-once there, model the side-effecting action as
a code step (so the runtime mediates the key) or thread a key into the agent via
`with:`.

# EXAMPLE

A CVE triage -> exploit -> PR pipeline. A multi-service lab stays up across
stages; the gate wraps the exploit in an independent validator with a
benign-payload oracle and repairs on failure; a hard reject is caught and turned
into a clean exit; a human approves before the PR; the PR is idempotent; the lab
is torn down with the run.

    workflow: cve-pipeline
    version: 1
    input:
      type: object
      required: [cve_id]
      properties: { cve_id: { type: string } }

    containers:
      lab:                                  # services: vulnerable, patched, db, capture
        compose: ./lab/compose.yml          # readiness via compose healthchecks + up --wait
        service: vulnerable

    graph:
      - id: triage
        container: lab
        uses: anthropic/claude-code
        with: { skill: cve-triage, cve: "{{ input.cve_id }}" }
        output_schema:
          type: object
          additionalProperties: false
          required: [web_exploitable, has_source]
          properties:
            web_exploitable: { type: boolean }
            has_source:      { type: boolean }

      # skip-and-exit guards keep the main line flat
      - if:
          cond: "{{ !(step.triage.web_exploitable && step.triage.has_source) }}"
          then: [ - skip: "not web-exploitable or no source" ]

      - try:
          do:
            - gate:                          # exploit, judged independently, repaired on failure
                generate:
                  - id: exploit              # repair attempts auto-receive the prior verdict
                    container: lab
                    uses: anthropic/claude-code
                    with: { skill: cve-exploit }
                evaluate:                     # multi-step independent judge
                  - id: run_oracle            # deterministic: exploit on vuln + patched + benign payload
                    container: lab
                    run: ./validate.sh "{{ input.cve_id }}"
                    output_files: [/out/oracle.har]
                    output_schema:
                      type: object
                      additionalProperties: false
                      required: [verified, detections, false_positives, feedback]
                      properties:
                        verified:        { type: boolean }
                        detections:      { type: integer }
                        false_positives: { type: integer }
                        feedback:        { type: string }
                until: "{{ evaluate.verified && evaluate.detections == 5 && evaluate.false_positives == 0 }}"
                max_attempts: 5

            - id: approve                    # human gate before an external effect
              await: human_review
              timeout: 24h
              output_schema:
                type: object
                required: [approved]
                properties: { approved: { type: boolean } }

            - if:
                cond: "{{ !step.approve.approved }}"
                then: [ - skip: "human rejected the exploit" ]

            - id: open_pr
              container: lab
              run: ./open-pr.sh "{{ input.cve_id }}"
              idempotency_key: "{{ input.cve_id }}:pr"

          catch:                             # gate exhausted max_attempts -> exit cleanly
            - skip: "no validated exploit after repair budget"

A map->map chain. Map A scans each input host and produces a typed `finding`;
map B fans out over A's aggregated findings — `over: "{{ step.scan }}"` is the
index-ordered array of A's per-item `scan` outputs, each element bound as `f`.

    workflow: scan-then-verify
    version: 1
    input:
      type: object
      required: [hosts]
      properties:
        hosts: { type: array, items: { type: string } }

    containers:
      lab:
        image: oci://example.com/scanner@sha256:0000000000000000000000000000000000000000000000000000000000000000

    graph:
      - map:                                 # map A: one scan per host
          over: "{{ input.hosts }}"
          as: h
          container: lab
          concurrency: 4
          body:
            - id: scan
              container: lab
              run: ./scan.sh "{{ h }}"
              output_schema:
                type: object
                additionalProperties: false
                required: [finding]
                properties:
                  finding: { type: string }

      - map:                                 # map B: one verify per aggregated finding
          over: "{{ step.scan }}"            # []scan-output, in item-index order
          as: f
          container: lab
          concurrency: 4
          body:
            - id: verify
              container: lab
              run: ./verify.sh "{{ f.finding }}"

# SEE ALSO

**awf**(1), and the project README for an introduction to AWF.
