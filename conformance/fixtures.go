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
