package conformance

import "fmt"

var subworkflowSimpleRootWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-root
version: 1
imports:
  child: child.awf.yaml
containers:
  lab:
    image: %[1]s
graph:
  - id: child_call
    call: child
    input:
      topic: "alpha"
  - id: parent
    container: lab
    run: "./parent.sh {{ step.child_call.summary }}"
    retry: { attempts: 1 }
`, fakeImageDigest)

var subworkflowArtifactRootWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-artifact-root
version: 1
imports:
  child: child.awf.yaml
containers:
  lab:
    image: %[1]s
graph:
  - id: child_call
    call: child
  - id: consume
    container: lab
    run: "./consume-report.sh"
    retry: { attempts: 1 }
    input_files:
      /work/report.md: step.child_call.files.report
`, fakeImageDigest)

var subworkflowArtifactChildWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-artifact-child
version: 1
output_files:
  report: step.final.files.report
containers:
  lab:
    image: %[1]s
graph:
  - id: final
    container: lab
    run: "./make-report.sh"
    retry: { attempts: 1 }
    output_files:
      report: /out/report.md
`, fakeImageDigest)

var subworkflowAggregateRootWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-aggregate-root
version: 1
imports:
  child: child.awf.yaml
containers:
  lab:
    image: %[1]s
graph:
  - id: child_call
    call: child
    input:
      items: ["a", "b"]
  - id: consume
    container: lab
    run: "./consume-aggregate.sh"
    retry: { attempts: 1 }
    input_files:
      /work/versions.csv: step.child_call.files.item4
`, fakeImageDigest)

var subworkflowAggregateChildWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-aggregate-child
version: 1
input:
  type: object
  additionalProperties: false
  required: [items]
  properties:
    items:
      type: array
      items: { type: string }
output_files:
  item4: step.version_universe.files.item4
containers:
  lab:
    image: %[1]s
  agg:
    image: %[1]s
graph:
  - map:
      id: version_universe
      over: "{{ input.items }}"
      as: x
      container: lab
      concurrency: 1
      body:
        - id: row
          container: lab
          run: "./row.sh {{ x }}"
          retry: { attempts: 1 }
          output_files:
            leaf: /out/leaf.csv
      reduce:
        run: "./merge.sh"
        container: agg
        output_schema:
          type: object
          additionalProperties: false
          required: [csv_rows]
          properties:
            csv_rows: { type: integer }
        output_files:
          item4: /out/versions.csv
`, fakeImageDigest)

var subworkflowAssetCollisionRootWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-asset-collision-root
version: 1
assets:
  schema: root/schema.json
imports:
  child: child.awf.yaml
containers:
  lab:
    image: %[1]s
graph:
  - id: child_call
    call: child
  - id: root_consume
    container: lab
    run: "./consume-root-schema.sh"
    retry: { attempts: 1 }
    input_files:
      /work/root-schema.json: asset.schema
`, fakeImageDigest)

var subworkflowAssetCollisionChildWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-asset-collision-child
version: 1
assets:
  schema: child/schema.json
containers:
  lab:
    image: %[1]s
graph:
  - id: child_consume
    container: lab
    run: "./consume-child-schema.sh"
    retry: { attempts: 1 }
    input_files:
      /work/child-schema.json: asset.schema
`, fakeImageDigest)

var subworkflowRepeatedRootWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-repeated-root
version: 1
imports:
  child: child.awf.yaml
containers:
  lab:
    image: %[1]s
graph:
  - id: first_call
    call: child
    input:
      topic: "one"
  - id: second_call
    call: child
    input:
      topic: "two"
  - id: combine
    container: lab
    run: "./combine.sh {{ step.first_call.summary }} {{ step.second_call.summary }}"
    retry: { attempts: 1 }
`, fakeImageDigest)

var subworkflowNestedRootWorkflow = `workflow: conformance-subworkflow-nested-root
version: 1
imports:
  outer: outer.awf.yaml
containers: {}
graph:
  - id: outer_call
    call: outer
`

var subworkflowNestedOuterWorkflow = `workflow: conformance-subworkflow-nested-outer
version: 1
imports:
  inner: inner.awf.yaml
output_schema:
  type: object
  additionalProperties: false
  required: [result]
  properties:
    result: { type: string }
outputs:
  result: "{{ step.inner_call.result }}"
containers: {}
graph:
  - id: inner_call
    call: inner
`

var subworkflowNestedInnerWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-nested-inner
version: 1
output_schema:
  type: object
  additionalProperties: false
  required: [result]
  properties:
    result: { type: string }
outputs:
  result: "{{ step.final.result }}"
containers:
  lab:
    image: %[1]s
graph:
  - id: final
    container: lab
    run: "./inner.sh"
    retry: { attempts: 1 }
    output_schema:
      type: object
      additionalProperties: false
      required: [result]
      properties:
        result: { type: string }
`, fakeImageDigest)

var subworkflowDriftRootWorkflow = `workflow: conformance-subworkflow-drift-root
version: 1
imports:
  child: child.awf.yaml
containers: {}
graph:
  - id: child_call
    call: child
`

func subworkflowDriftChildWorkflow(run string) string {
	return fmt.Sprintf(`workflow: conformance-subworkflow-drift-child
version: 1
containers:
  lab:
    image: %[1]s
graph:
  - id: final
    container: lab
    run: "%[2]s"
    retry: { attempts: 1 }
`, fakeImageDigest, run)
}

var subworkflowAssetDriftRootWorkflow = `workflow: conformance-subworkflow-asset-drift-root
version: 1
imports:
  child: child.awf.yaml
containers: {}
graph:
  - id: child_call
    call: child
`

var subworkflowAssetDriftChildWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-asset-drift-child
version: 1
assets:
  schema: child/schema.json
containers:
  lab:
    image: %[1]s
graph:
  - id: final
    container: lab
    run: "./child.sh"
    retry: { attempts: 1 }
    input_files:
      /work/schema.json: asset.schema
`, fakeImageDigest)

var subworkflowSimpleChildWorkflow = fmt.Sprintf(`workflow: conformance-subworkflow-child
version: 1
input:
  type: object
  additionalProperties: false
  required: [topic]
  properties:
    topic: { type: string }
output_schema:
  type: object
  additionalProperties: false
  required: [summary]
  properties:
    summary: { type: string }
outputs:
  summary: "{{ step.final.summary }}"
containers:
  lab:
    image: %[1]s
graph:
  - id: final
    container: lab
    run: "./child.sh {{ input.topic }}"
    retry: { attempts: 1 }
    output_schema:
      type: object
      additionalProperties: false
      required: [summary]
      properties:
        summary: { type: string }
`, fakeImageDigest)
