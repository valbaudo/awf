package conformance

const tinySeqWorkflow = `workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`

// fiveStepSeqWorkflow uses `retry: { attempts: 1 }` on every step — see
// slice 2.6 Design question 7. Bucket 2's `FailExecAfterN(k)` is one-shot
// per container.Fake's slice-2.4 contract; with retry.Default.Attempts=3
// the (k+2)-th call would succeed and the "crash" would be invisible.
// Pinning attempts=1 makes the fault actually halt the step.
const fiveStepSeqWorkflow = `workflow: conformance-five-step-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: s1
    container: lab
    run: "./s1.sh"
    retry: { attempts: 1 }
  - id: s2
    container: lab
    run: "./s2.sh"
    retry: { attempts: 1 }
  - id: s3
    container: lab
    run: "./s3.sh"
    retry: { attempts: 1 }
  - id: s4
    container: lab
    run: "./s4.sh"
    retry: { attempts: 1 }
  - id: s5
    container: lab
    run: "./s5.sh"
    retry: { attempts: 1 }
`

const tinySeqWorkflowMutated = `workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1-MUTATED.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`

// propagationCaughtWorkflow exercises Bucket 4a sub-test "caught": step2 is
// wrapped in try.catch.finally; step2's command always exits 1 (with
// retry: { attempts: 1 } so the one-shot fake fault halts the step on the
// first call). The catch absorbs the failure, finally runs, step3 runs, the
// run completes ok.
const propagationCaughtWorkflow = `workflow: conformance-propagation-caught
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
    retry: { attempts: 1 }
  - try:
      do:
        - id: step2-failing
          container: lab
          run: "./step2-failing.sh"
          retry: { attempts: 1 }
      catch:
        - id: catch-step
          container: lab
          run: "./catch.sh"
          retry: { attempts: 1 }
      finally:
        - id: finally-step
          container: lab
          run: "./finally.sh"
          retry: { attempts: 1 }
  - id: step3
    container: lab
    run: "./step3.sh"
    retry: { attempts: 1 }
`

// propagationUncaughtWorkflow is identical to propagationCaughtWorkflow
// EXCEPT step2 is NOT wrapped in try.catch — its failure propagates to the
// run root.
const propagationUncaughtWorkflow = `workflow: conformance-propagation-uncaught
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
    retry: { attempts: 1 }
  - id: step2-failing
    container: lab
    run: "./step2-failing.sh"
    retry: { attempts: 1 }
  - id: step3
    container: lab
    run: "./step3.sh"
    retry: { attempts: 1 }
`

// skipAtRootWorkflow exercises Bucket 6 sub-test "at_root": a single Skip
// node at the workflow root. The validator accepts an empty `containers: {}`
// for workflows where no step references a container — skip never invokes
// the dispatcher, so no container is needed (verified empirically).
const skipAtRootWorkflow = `workflow: conformance-skip-root
version: 1
containers: {}
graph:
  - skip: "early exit"
`

// skipInLoopBodyWorkflow exercises Bucket 6 sub-test "in_loop_body":
// loop{max_iters:3, body:[skip]} — each iter ends via skip, loop runs all
// 3 iters, 3 loop.iter + 3 node.skipped recorded. No container needed (the
// body never reaches a step that uses one).
const skipInLoopBodyWorkflow = `workflow: conformance-skip-loop
version: 1
containers: {}
graph:
  - loop:
      max_iters: 3
      body:
        - skip: "skip iter"
`

// skipInTryDoWorkflow exercises Bucket 6 sub-test "in_try_do": skip inside
// try.do bypasses Catch, runs Finally, and — per spec §5.6 — propagates past
// the try to the workflow root, preventing siblings AFTER the try from running.
// Steps that must NOT run are deliberately unprogrammed in the fake: the
// ProgramExec-miss error would fire and surface the spec violation.
const skipInTryDoWorkflow = `workflow: conformance-skip-try
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - try:
      do:
        - skip: "skip do"
      catch:
        - id: must-not-run-catch
          container: lab
          run: "./must-not-run-catch.sh"
          retry: { attempts: 1 }
      finally:
        - id: must-run-finally
          container: lab
          run: "./must-run-finally.sh"
          retry: { attempts: 1 }
  - id: must-not-run-after-try
    container: lab
    run: "./must-not-run-after-try.sh"
    retry: { attempts: 1 }
`

// parallelCancellationWorkflow — Bucket 4b parallel_cancellation: try.catch
// wraps a 3-branch parallel. Branch 0 fails (retry-exhausting); branches
// 1+2 are each try { do, finally }. Distinct containers (AWF1010).
const parallelCancellationWorkflow = `workflow: conformance-parallel-cancel
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
  c1:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
  c2:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - try:
      do:
        - parallel:
            - id: b0-failing
              container: c0
              run: "./b0-failing.sh"
              retry: { attempts: 1 }
            - try:
                do:
                  - id: b1-do
                    container: c1
                    run: "./b1-do.sh"
                    retry: { attempts: 1 }
                finally:
                  - id: b1-finally
                    container: c1
                    run: "./b1-finally.sh"
                    retry: { attempts: 1 }
            - try:
                do:
                  - id: b2-do
                    container: c2
                    run: "./b2-do.sh"
                    retry: { attempts: 1 }
                finally:
                  - id: b2-finally
                    container: c2
                    run: "./b2-finally.sh"
                    retry: { attempts: 1 }
      catch:
        - id: outer-catch
          container: c0
          run: "./outer-catch.sh"
          retry: { attempts: 1 }
`

// gateFeedbackThreadingWorkflow — Bucket 5 feedback_threading. Generator's
// run command interpolates {{ evaluate.feedback }}; the fake's ProgramExec
// can only program one result per command, so the test asserts dispatch
// SEQUENCE (attempt 1 with empty feedback, attempt 2 with "missing X" from
// attempt-1's verdict). max_attempts: 2 so both attempts run, both reject
// (eval returns same verdict each call), gate exhausts → OutcomeRejected.
// Pass-on-2 verdict-roundtrip is covered by the engine-level test.
const gateFeedbackThreadingWorkflow = `workflow: conformance-gate-feedback
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh {{ evaluate.feedback }}"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 2
`

// gateMaxAttemptsRejectedWorkflow — Bucket 5 max_attempts_rejected. Evaluator
// always returns verified:false; gate exhausts max_attempts:3 attempts and
// returns OutcomeRejected.
const gateMaxAttemptsRejectedWorkflow = `workflow: conformance-gate-max-attempts
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 3
`

// gateCrashNotVerdictWorkflow — Bucket 5 crash_not_verdict. Generator
// crashes (retry-exhausted) on attempt 1; gate must propagate WITHOUT
// committing a gate.attempt event. The evaluator script entry is deliberately
// programmed to verified:true — if it ran (it should NOT), the gate would
// pass on attempt 1 and the test would mis-judge as "no rejection."
const gateCrashNotVerdictWorkflow = `workflow: conformance-gate-crash
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen-crash.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 3
`

// gateMidResumeWorkflow — Bucket 5 mid_resume. max_attempts:5. Evaluator
// always returns verified:false on the first run, then is re-programmed to
// also keep failing on resume (5 total attempts → rejected). The test asserts
// the post-run + post-resume RunState has GateAttempts of length 5, with N
// monotonically increasing 1..5 across the run boundary.
const gateMidResumeWorkflow = `workflow: conformance-gate-mid-resume
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 5
`

// gateIndependencePlaceholderWorkflow — Bucket 5 independence_placeholder.
// Trivial gate (2-step generate, 1-step evaluator). The test asserts the
// recordingDispatcher captured 3 DISTINCT NodeIntent paths (gen1, gen2, eval1)
// — the Phase 5 fresh-context proof replaces this assertion when the
// agent.Adapter lands.
const gateIndependencePlaceholderWorkflow = `workflow: conformance-gate-independence
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen1.sh"
          retry: { attempts: 1 }
        - id: gen2
          container: c0
          run: "./gen2.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 1
`

// gateRejectedCaughtWorkflow — Bucket 5 rejected_caught_by_try (critique-pass
// addition). Gate wrapped in try.catch; gate rejects (eval returns verified:false,
// max_attempts:1); catch absorbs; handler step runs; run completes ok.
const gateRejectedCaughtWorkflow = `workflow: conformance-gate-rejected-caught
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - try:
      do:
        - gate:
            generate:
              - id: gen1
                container: c0
                run: "./gen.sh"
                retry: { attempts: 1 }
            evaluate:
              - id: eval1
                container: c0
                run: "./eval.sh"
                retry: { attempts: 1 }
                output_schema:
                  type: object
                  additionalProperties: false
                  required: [verified, feedback]
                  properties:
                    verified: { type: boolean }
                    feedback: { type: string }
            until: "{{ evaluate.verified }}"
            max_attempts: 1
      catch:
        - id: handler
          container: c0
          run: "./handler.sh"
          retry: { attempts: 1 }
`

// parallelResumeWorkflow — Bucket 4b parallel_resume_consistency:
// simple 3-branch parallel followed by a sequential after-step. The test
// programs pb2.sh to fail deterministically on first run, then re-programs
// it to succeed on resume. Expectation: pb0+pb1 commit pre-crash, pb2
// doesn't; resume skips committed (replays from log), re-runs only pb2 +
// after.
const parallelResumeWorkflow = `workflow: conformance-parallel-resume
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
  c1:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
  c2:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - parallel:
      - id: pb0
        container: c0
        run: "./pb0.sh"
        retry: { attempts: 1 }
      - id: pb1
        container: c1
        run: "./pb1.sh"
        retry: { attempts: 1 }
      - id: pb2
        container: c2
        run: "./pb2.sh"
        retry: { attempts: 1 }
  - id: after
    container: c0
    run: "./after.sh"
    retry: { attempts: 1 }
`
