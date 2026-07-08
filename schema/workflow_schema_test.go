package schema

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/valbaudo/awf/frontend/yaml"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// schemaFile is the hand-authored structural schema this test pins to the IR.
const schemaFile = "workflow.v1.schema.json"

// compileSchema loads and compiles workflow.v1.schema.json, mirroring the v6 usage
// in ir/validate_schema.go (NewCompiler → AddResource → Compile).
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open(schemaFile)
	if err != nil {
		t.Fatalf("open %s: %v", schemaFile, err)
	}
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse %s: %v", schemaFile, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaFile, doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile(schemaFile)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaFile, err)
	}
	return sch
}

// validateJSON validates raw JSON bytes against the compiled schema, decoding the
// instance with json.Number precision (the v6-recommended path).
func validateJSON(sch *jsonschema.Schema, raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return sch.Validate(inst)
}

// corpusFiles is the positive corpus: known-good, runnable example workflows plus the
// loader/ir valid goldens. It DELIBERATELY excludes ir/testdata/invalid/* (those are
// intentionally-malformed validator fixtures) — the walk/globs never descend into it.
//
// examples/ is walked recursively (filepath.WalkDir) so deeper-nested examples are not
// silently dropped, and any *.yaml under it that is not a workflow document (a stray
// compose/fixture file) is skipped rather than hard-failing the suite — only files whose
// top level carries a `workflow:` key are kept. The two curated testdata sources are
// trusted workflows and are taken verbatim. Each source is asserted non-empty on its own
// (not just the aggregate), so a broken path can't silently shrink the corpus to nothing.
func corpusFiles(t *testing.T) []string {
	t.Helper()

	var examples []string
	if err := filepath.WalkDir("../examples", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return nil
		}
		if isWorkflowDoc(p) {
			examples = append(examples, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk ../examples: %v", err)
	}
	loaderValid := globCorpus(t, "../loader/testdata/valid/*.yaml") // cve-pipeline.yaml
	irValid := globCorpus(t, "../ir/testdata/*.yaml")               // valid top-level goldens (invalid/ is a subdir)

	for name, src := range map[string][]string{
		"examples":              examples,
		"loader/testdata/valid": loaderValid,
		"ir/testdata":           irValid,
	} {
		if len(src) == 0 {
			t.Fatalf("corpus source %q is empty — check its path", name)
		}
	}

	all := append([]string{}, examples...)
	all = append(all, loaderValid...)
	all = append(all, irValid...)
	return all
}

func globCorpus(t *testing.T, pat string) []string {
	t.Helper()
	m, err := filepath.Glob(pat)
	if err != nil {
		t.Fatalf("glob %q: %v", pat, err)
	}
	return m
}

// isWorkflowDoc reports whether the YAML file at p is a workflow document — its top-level
// mapping carries a `workflow:` key. Used to skip stray non-workflow YAML under examples/
// (compose files, fixtures) without hard-failing. A parse error means "not a workflow doc".
func isWorkflowDoc(p string) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	_, raw, err := yaml.DecodeWithRaw(b)
	if err != nil || raw == nil {
		return false
	}
	_, ok := raw["workflow"]
	return ok
}

// TestSchemaAcceptsValidCorpus loads every valid corpus workflow, marshals its IR to
// the wire JSON, and asserts the schema accepts it.
//
// Coverage is honest about its limits: the runnable example/testdata corpus exercises
// only a subset of the 13 node kinds (today: run, uses, call, gate, parallel) and none
// of the custom-marshaler wire variants. The `additionalProperties:false` pinning
// therefore blocks silent IR drift ONLY for the shapes something here actually marshals
// — a field added to, say, a react or map step drifts undetected by THIS test. The other
// 8 kinds (await, if, loop, try, skip, map, compose, react) and every marshaler variant
// are pinned solely by the hand-authored allKindsWorkflow fixture in
// TestSchemaAcceptsAllNodeKinds; keep that fixture exhaustive.
func TestSchemaAcceptsValidCorpus(t *testing.T) {
	sch := compileSchema(t)
	for _, f := range corpusFiles(t) {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			ld, err := loader.Load(f)
			if err != nil {
				t.Fatalf("load %s: %v", f, err)
			}
			raw, err := json.Marshal(ld.Workflow)
			if err != nil {
				t.Fatalf("marshal %s: %v", f, err)
			}
			if err := validateJSON(sch, raw); err != nil {
				t.Fatalf("schema rejected valid workflow %s:\n%v\nwire JSON:\n%s", f, err, raw)
			}
		})
	}
}

// TestSchemaAcceptsAuthoredYAML validates each corpus workflow in its AUTHORED form —
// the raw YAML document converted DIRECTLY to JSON through the same YAML library the
// loader uses (frontend/yaml, goccy under the hood), with no round-trip through the IR
// marshalers. This is the actual editor/CI use-case the schema exists for, and it reaches
// wire arms the marshaled-IR pass never does: string durations ("5m") instead of integer
// nanoseconds, sugar/omitted fields, and top-level x-* YAML-anchor holders. A valid
// authored example that the schema rejects is a real schema defect for its stated purpose.
func TestSchemaAcceptsAuthoredYAML(t *testing.T) {
	sch := compileSchema(t)
	for _, f := range corpusFiles(t) {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			// Stage 1+2 of the loader's decode pipeline: YAML → any (goccy) → JSON, giving
			// the authored document verbatim (string durations, x-*, sugar) — no IR pass.
			_, raw, err := yaml.DecodeWithRaw(b)
			if err != nil {
				t.Fatalf("decode %s: %v", f, err)
			}
			authored, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("marshal authored %s: %v", f, err)
			}
			if err := validateJSON(sch, authored); err != nil {
				t.Fatalf("schema rejected AUTHORED workflow %s (real schema defect):\n%v\nauthored JSON:\n%s", f, err, authored)
			}
		})
	}
}

// TestSchemaAcceptsAllNodeKinds builds one in-Go Workflow exercising all 13 node kinds
// AND every custom-marshaler wire shape (Duration int/string, Timeout scalar/{wall,idle},
// Map.over string/array, output_files list/map, Reduce quorum/run, Prune keep/stop_when,
// Skip/Parallel wrappers), marshals it through the real IR marshalers, and validates it.
// This is the "conformance-golden, all 13 kinds" row of the spec I/O matrix, and it
// exercises every schema branch the real corpus does not reach.
func TestSchemaAcceptsAllNodeKinds(t *testing.T) {
	sch := compileSchema(t)
	wf := allKindsWorkflow()

	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal all-kinds: %v", err)
	}
	if err := validateJSON(sch, raw); err != nil {
		t.Fatalf("schema rejected all-kinds workflow:\n%v\nwire JSON:\n%s", err, raw)
	}

	// Assert the fixture actually exercises all 13 kind keys on the wire (some kinds,
	// e.g. skip, appear only nested inside a control node's body). Collect kind keys ONLY
	// from graph/body NODE positions — NOT every object key anywhere — so a `run`/`uses`/
	// `await`/`call` key that is really a struct field (ToolImpl.Run, Reduce.Run,
	// SignalStep.Await, CallStep.Call, ...) cannot masquerade as a node of that kind and
	// satisfy the "all 13 present" assertion without the actual node.
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	collectNodeKinds(doc, false, seen)
	stepKeys := []string{"run", "uses", "await", "call"}
	controlKeys := []string{"if", "loop", "try", "parallel", "gate", "skip", "map", "compose", "react"}
	for _, k := range append(append([]string{}, stepKeys...), controlKeys...) {
		if !seen[k] {
			t.Errorf("all-kinds fixture missing node kind %q on the wire", k)
		}
	}
}

// TestSchemaRejectsUnknownTopLevelKey pins the structural strictness: an unknown
// top-level key (which AWF rejects semantically as AWF1062) must fail the schema via
// root additionalProperties:false.
func TestSchemaRejectsUnknownTopLevelKey(t *testing.T) {
	sch := compileSchema(t)
	raw := []byte(`{"workflow":"x","version":1,"graph":[],"bogus_key":true}`)
	if err := validateJSON(sch, raw); err == nil {
		t.Fatal("schema accepted a workflow with an unknown top-level key; want rejection")
	}
}

// TestSchemaRejectsUnknownNodeKind pins the node oneOf: an object matching none of the
// 13 key-presence branches must fail (I/O matrix "Not a valid kind" row).
func TestSchemaRejectsUnknownNodeKind(t *testing.T) {
	sch := compileSchema(t)
	raw := []byte(`{"workflow":"x","version":1,"graph":[{"id":"n","frobnicate":"y"}]}`)
	if err := validateJSON(sch, raw); err == nil {
		t.Fatal("schema accepted a graph node with no valid kind key; want rejection")
	}
}

// TestSchemaRejectsInvalidDocuments proves the strictness that is the schema's whole value:
// a wrong-typed scalar, a typo'd field inside a node (the exact case additionalProperties:false
// targets), and a missing required field are each REJECTED. If any of these were accepted the
// schema would be a shape sieve with holes.
func TestSchemaRejectsInvalidDocuments(t *testing.T) {
	sch := compileSchema(t)
	cases := []struct {
		name string
		raw  string
	}{
		{
			// version is `const: 1` (integer); the string "1" is the wrong scalar type.
			name: "wrong-typed scalar (version as string)",
			raw:  `{"workflow":"x","version":"1","graph":[]}`,
		},
		{
			// A code step with a typo'd `otput_schema`: codeStep is additionalProperties:false
			// and the object matches no other node branch, so the node oneOf fails.
			name: "typo'd field inside a node (otput_schema)",
			raw:  `{"workflow":"x","version":1,"graph":[{"id":"c","run":"echo hi","otput_schema":{}}]}`,
		},
		{
			// Root requires workflow/version/graph; graph is absent.
			name: "missing required field (graph)",
			raw:  `{"workflow":"x","version":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateJSON(sch, []byte(tc.raw)); err == nil {
				t.Fatalf("schema accepted an invalid document (%s); want rejection:\n%s", tc.name, tc.raw)
			}
		})
	}
}

// nodeKindKeys are the 13 keys that discriminate a graph node's kind (mirrors
// ir.unmarshalNode): 4 flat step kinds + 9 control kinds.
var nodeKindKeys = map[string]bool{
	"run": true, "uses": true, "await": true, "call": true,
	"if": true, "loop": true, "try": true, "parallel": true, "gate": true,
	"skip": true, "map": true, "compose": true, "react": true,
}

// nodeListKeys are the keys whose value is a node list (the `oneOf` node slots), i.e. the
// positions whose array elements are graph nodes. `parallel` is both a kind key and a
// node-list key: a parallel node IS `{parallel: [<node>...]}`, its value the inner list.
var nodeListKeys = map[string]bool{
	"graph": true, "then": true, "else": true, "body": true, "do": true,
	"catch": true, "finally": true, "generate": true, "evaluate": true, "parallel": true,
}

// collectNodeKinds records node-kind keys ONLY from objects sitting in a node-list
// position (inNodeList), never from arbitrary object keys. This makes the "all 13 kinds
// present" assertion meaningful: a kind counts only when a genuine node of that kind
// exists in a graph/body slot — a `run` key on a reduce/tool impl, or an `await`/`call`
// struct field, is never in a node-list position, so it cannot spoof a node kind.
func collectNodeKinds(v any, inNodeList bool, into map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if inNodeList { // this object is a graph node — its kind key(s) count
			for k := range t {
				if nodeKindKeys[k] {
					into[k] = true
				}
			}
		}
		for k, sub := range t {
			collectNodeKinds(sub, nodeListKeys[k], into)
		}
	case []any:
		for _, e := range t {
			collectNodeKinds(e, inNodeList, into)
		}
	}
}

// --- fixtures ---

func dur(s string) *ir.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	x := ir.Duration(d)
	return &x
}
func boolp(b bool) *bool         { return &b }
func tmpl(s string) *ir.Template { x := ir.Template(s); return &x }
func exprp(s string) *ir.Expr    { x := ir.Expr(s); return &x }
func intp(n int) *int            { return &n }
func ratiop(s string) *ir.Ratio  { x := ir.Ratio(s); return &x }

// allKindsWorkflow constructs a structurally-shaped Workflow covering all 13 node kinds
// and the custom-marshaler variants. It need not pass semantic validation (awf validate);
// it only has to marshal and match the STRUCTURAL schema.
func allKindsWorkflow() ir.Workflow {
	sch := ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
	}
	return ir.Workflow{
		ID:          "kitchen-sink",
		Version:     1,
		InputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": true},
		InputFiles:  ir.WorkflowInputFiles{"spec": {Format: "text", SchemaRef: "step.x.files.y"}},
		Env:         []string{"FOO", "BAR"},
		Assets:      map[string]string{"a": "./a.txt"},
		Skills:      map[string]ir.SkillCorpus{"s": {From: "./skills", Layout: "flat", Router: "bm25"}},
		Imports:     map[string]string{"child": "./child.yaml"},
		Agents:      map[string]ir.AgentRole{"worker": {Uses: "awf/llm", Model: "m", SystemPrompt: "sp", With: ir.RawConfig{"provider": "anthropic"}}},
		Containers: map[string]ir.Container{
			"lab":    {Compose: "./lab/compose.yml", Service: "vuln", Snapshot: "workspace", Cmd: []string{"sleep", "inf"}, Keepalive: boolp(true), Resources: &ir.Resources{CPU: "2", Mem: "4Gi"}},
			"runner": {Image: "oci://x@sha256:1111"},
		},
		OutputSchema:    &sch,
		Outputs:         map[string]ir.TemplateValue{"result": json.RawMessage(`"{{ step.gen.field }}"`)},
		ArtifactExports: ir.ArtifactExports{"final": "step.gen.files.out"},
		Tools: map[string]ir.Tool{
			"grep": {Description: "search", InputSchema: &sch, Impl: ir.ToolImpl{Run: "./grep.sh", Container: "runner", Timeout: dur("30s"), InputFiles: map[string]string{"/in": "asset.a"}, Retry: &ir.RetryPolicy{Attempts: 2}}},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "code", Container: "runner", Run: "./run.sh", Timeout: &ir.Timeout{Wall: dur("5m")}, OutputSchema: &sch, OutputFiles: ir.OutputFiles{{Path: "/out/a"}}, InputFiles: map[string]string{"/in/x": "asset.a"}, IdempotencyKey: tmpl("k-{{ input.id }}"), Retry: &ir.RetryPolicy{Attempts: 3, Backoff: "exponential", Initial: dur("1s"), Max: dur("30s"), NonRetryableExitCodes: []int{2}, Recovery: "restart"}},
			&ir.AgentStep{ID: "gen", Uses: "worker", With: ir.RawConfig{"prompt": "hi", "nested": map[string]any{"k": 1}}, OutputSchema: &sch, OutputArtifact: "out", Timeout: &ir.Timeout{Wall: dur("2m"), Idle: dur("30s")}, Skills: &ir.StepSkillRouting{From: "s", Query: "q", Limit: 3, Into: "skills"}},
			&ir.AgentStep{ID: "gen2", Uses: "worker", Continues: "gen", OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/r.md"}, {Name: "typed", Path: "/out/t.json", Format: "json", SchemaRef: "step.x.files.y"}}},
			&ir.SignalStep{ID: "sig", Await: "approval", Where: "{{ signal.id == input.id }}", Timeout: dur("1h"), OutputSchema: &sch},
			&ir.CallStep{ID: "callit", Call: "child", Input: map[string]ir.TemplateValue{"cve": json.RawMessage(`"{{ input.cve }}"`)}, InputFiles: map[string]string{"/in": "step.code.files.a"}},
			&ir.If{Cond: "{{ input.flag }}", Then: ir.NodeList{&ir.Skip{Reason: "noop"}}, Else: ir.NodeList{&ir.Skip{Reason: ""}}},
			&ir.Loop{Until: exprp("{{ step.code.done }}"), MaxIters: intp(3), Body: ir.NodeList{&ir.CodeStep{ID: "l", Container: "runner", Run: "x"}}},
			&ir.Try{Do: ir.NodeList{&ir.CodeStep{ID: "t1", Container: "runner", Run: "x"}}, Catch: ir.NodeList{&ir.CodeStep{ID: "t2", Container: "runner", Run: "y"}}, Finally: ir.NodeList{&ir.CodeStep{ID: "t3", Container: "runner", Run: "z"}}},
			&ir.Parallel{Children: ir.NodeList{&ir.CodeStep{ID: "p1", Container: "runner", Run: "a"}, &ir.CodeStep{ID: "p2", Container: "runner", Run: "b"}}},
			&ir.Gate{MaxAttempts: 3, Generate: ir.NodeList{&ir.AgentStep{ID: "draft", Uses: "worker", OutputSchema: &sch, OutputArtifact: "d"}}, Evaluate: ir.NodeList{&ir.AgentStep{ID: "judge", Uses: "worker", OutputSchema: &sch}}, Until: "{{ evaluate.ok }}"},
			&ir.Map{ID: "m1", Over: "{{ step.code.items }}", As: "item", Container: "runner", Image: "oci://x@sha256:2222", Concurrency: intp(4), MinSuccess: ratiop("0.8"), Body: ir.NodeList{&ir.CodeStep{ID: "mi", Container: "runner", Run: "score.sh", OutputSchema: &sch}}, Reduce: &ir.Reduce{Quorum: ratiop("2"), Field: "ok", OutputSchema: &sch}, Prune: &ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 3}}},
			&ir.Map{ID: "m2", OverItems: []any{"a", "b", map[string]any{"x": 1}}, As: "it", Container: "runner", Body: ir.NodeList{&ir.CodeStep{ID: "mi2", Container: "runner", Run: "x"}}, Reduce: &ir.Reduce{Run: "reduce.sh", Container: "runner", OutputFiles: ir.OutputFiles{{Name: "agg", Path: "/out/agg.json"}}}, Prune: &ir.Prune{Score: "score", StopWhen: "{{ best.score > 0.9 }}"}},
			&ir.Compose{As: "svc", From: "lab", Service: "{{ input.svc }}", Body: ir.NodeList{&ir.CodeStep{ID: "c1", Container: "runner", Run: "x"}}},
			&ir.React{ID: "r", With: ir.RawConfig{"provider": "anthropic"}, Prompt: "solve", Tools: []string{"grep"}, MaxTurns: 5, OutputSchema: &sch},
		},
	}
}
