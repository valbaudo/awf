package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func liveRegistry(t *testing.T, refs ...string) *agent.Registry {
	t.Helper()
	var reg agent.Registry
	for _, ref := range refs {
		if err := reg.Register(fake.New(ref).WithCaps(agent.Caps{Containerless: true, PersistentSession: true})); err != nil {
			t.Fatalf("Register(%s): %v", ref, err)
		}
	}
	return &reg
}

func TestPersistentSessionRejectedInGateEvaluateRoot(t *testing.T) {
	reg := liveRegistry(t, "live/agent")
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/agent"}},
			Evaluate: ir.NodeList{
				&ir.AgentStep{ID: "judge", Uses: "live/agent"},
			},
		},
	}}

	err := checkPersistentSessionGateEvaluate(wf, reg)
	var want *ErrPersistentSessionGateEvaluate
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrPersistentSessionGateEvaluate", err)
	}
	if want.StepID != "judge" || want.Ref != "live/agent" {
		t.Fatalf("got %+v, want {StepID:judge Ref:live/agent}", want)
	}
}

func TestPersistentSessionRejectedInGateEvaluateImport(t *testing.T) {
	child := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/agent"}},
			Evaluate: ir.NodeList{&ir.AgentStep{ID: "judge", Uses: "live/agent"}},
		},
	}}
	ld := &ir.LoadedDefinition{
		Workflow: &ir.Workflow{},
		Modules: map[string]*ir.LoadedModule{
			"":      {ID: "", Workflow: &ir.Workflow{}},
			"child": {ID: "child", Workflow: child},
		},
	}
	reg := liveRegistry(t, "live/agent")

	err := checkPersistentSessionGateEvaluateForLoadedDefinition(ld, reg)
	var want *ErrPersistentSessionGateEvaluate
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrPersistentSessionGateEvaluate", err)
	}
	if !strings.Contains(err.Error(), "module child") {
		t.Fatalf("err = %v, want module child context", err)
	}
}

func TestPersistentSessionRejectedInGateEvaluateCallImport(t *testing.T) {
	root := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.CodeStep{ID: "gen", Container: "lab", Run: "true"}},
			Evaluate: ir.NodeList{&ir.CallStep{ID: "audit_call", Call: "audit"}},
		},
	}}
	child := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "judge", Uses: "live/agent"},
	}}
	ld := &ir.LoadedDefinition{
		Workflow: root,
		Modules: map[string]*ir.LoadedModule{
			"":      {ID: "", Workflow: root},
			"child": {ID: "child", Workflow: child},
		},
		ImportEdges: []ir.LoadedImportEdge{
			{ParentID: "", ImportID: "audit", ChildID: "child"},
		},
	}
	reg := liveRegistry(t, "live/agent")

	err := checkPersistentSessionGateEvaluateForLoadedDefinition(ld, reg)
	var want *ErrPersistentSessionGateEvaluate
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrPersistentSessionGateEvaluate", err)
	}
	if want.StepID != "judge" || want.Ref != "live/agent" {
		t.Fatalf("got %+v, want {StepID:judge Ref:live/agent}", want)
	}
}

func TestPersistentSessionRejectedInGateEvaluateRole(t *testing.T) {
	wf := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			"judge_role": {Uses: "live/base"},
		},
		Graph: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/base"}},
				Evaluate: ir.NodeList{&ir.AgentStep{ID: "judge", Uses: "judge_role"}},
			},
		},
	}
	base := fake.New("live/base").WithCaps(agent.Caps{Containerless: true, PersistentSession: true})
	derived := agent.NewDerivedAdapter("judge_role", base, nil)
	var reg agent.Registry
	if err := reg.Register(derived); err != nil {
		t.Fatalf("Register derived role: %v", err)
	}
	if err := reg.Register(fake.New("live/base").WithCaps(agent.Caps{Containerless: true})); err != nil {
		t.Fatalf("Register generate adapter: %v", err)
	}

	err := checkPersistentSessionGateEvaluate(wf, &reg)
	var want *ErrPersistentSessionGateEvaluate
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrPersistentSessionGateEvaluate", err)
	}
	if want.Ref != "judge_role" {
		t.Fatalf("Ref = %q, want judge_role", want.Ref)
	}
}

func TestPersistentSessionRejectedInGateEvaluateMapLoopCompose(t *testing.T) {
	reg := liveRegistry(t, "live/agent")
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/agent"}},
			Evaluate: ir.NodeList{
				&ir.Map{Body: ir.NodeList{
					&ir.Loop{Body: ir.NodeList{
						&ir.Compose{Body: ir.NodeList{
							&ir.AgentStep{ID: "judge", Uses: "live/agent"},
						}},
					}},
				}},
			},
		},
	}}

	err := checkPersistentSessionGateEvaluate(wf, reg)
	var want *ErrPersistentSessionGateEvaluate
	if !errors.As(err, &want) || want.StepID != "judge" {
		t.Fatalf("err = %v, want *ErrPersistentSessionGateEvaluate{StepID:judge}", err)
	}
}

func TestLiveGateGenerateAllowedEvaluateRejected(t *testing.T) {
	reg := liveRegistry(t, "live/agent")
	allowed := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/agent"}},
			Evaluate: ir.NodeList{&ir.CodeStep{ID: "judge", Container: "lab", Run: "true"}},
		},
	}}
	if err := checkPersistentSessionGateEvaluate(allowed, reg); err != nil {
		t.Fatalf("generate-only live adapter rejected: %v", err)
	}

	rejected := &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.AgentStep{ID: "gen", Uses: "live/agent"}},
			Evaluate: ir.NodeList{&ir.AgentStep{ID: "judge", Uses: "live/agent"}},
		},
	}}
	if err := checkPersistentSessionGateEvaluate(rejected, reg); err == nil {
		t.Fatal("checkPersistentSessionGateEvaluate = nil, want rejection")
	}
}

func TestResumeRejectsPersistentEvaluate(t *testing.T) {
	stateDir := t.TempDir()
	runID := "persistent-resume"
	wfPath := filepath.Join(stateDir, "workflow.yaml")
	wfYAML := `workflow: persistent-resume
version: 1
containers: {}
graph:
  - gate:
      generate:
        - id: gen
          uses: live/agent
      evaluate:
        - id: judge
          uses: live/agent
          output_schema:
            type: object
            required: [verified]
            properties:
              verified: {type: boolean}
      until: "{{ evaluate.verified }}"
      max_attempts: 1
`
	if err := os.WriteFile(wfPath, []byte(wfYAML), 0o644); err != nil {
		t.Fatalf("WriteFile workflow: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("workflow invalid: %v", diags)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	log, err := state.OpenLogExclusive(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	startedData, err := json.Marshal(engine.RunStartedData{
		RunID:          runID,
		WorkflowDigest: digest,
		Backend:        engine.BackendFake,
		Runtimes:       []engine.ResolvedRuntime{{Ref: "live/agent", Version: "fake-v1"}},
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: startedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	runner := &Runner{Backend: container.NewFake(), Resolver: liveRegistry(t, "live/agent"), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	if rc == ExitOK {
		t.Fatalf("rc = %d, want non-zero", rc)
	}
	if !strings.Contains(stderr.String(), "persistent session") || !strings.Contains(stderr.String(), "gate.evaluate") {
		t.Fatalf("stderr = %q, want persistent gate.evaluate rejection", stderr.String())
	}
}
