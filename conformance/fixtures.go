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
