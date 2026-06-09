package engine_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
)

func skillWorkflowFixture(t *testing.T, query ir.Template, mutate func(*ir.Workflow)) (*ir.LoadedDefinition, map[string]engine.RunStartedAsset, state.Blobs) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	assets := storeTestSkillAssets(t, blobs)
	wf := &ir.Workflow{
		ID:      "skills-agent",
		Version: 1,
		Assets:  map[string]string{"skill_assets": "skills"},
		Skills: map[string]ir.SkillCorpus{
			"awf": {From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName},
		},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:        "hunt",
				Container: "lab",
				Uses:      "anthropic/claude-code",
				With:      ir.RawConfig{"prompt": "go"},
				Skills:    &ir.StepSkillRouting{From: "awf", Query: query, Limit: 1, Into: "/skills"},
				OutputSchema: &ir.JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"ok"},
					"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
				},
			},
		},
	}
	if mutate != nil {
		mutate(wf)
	}
	return &ir.LoadedDefinition{Workflow: wf}, assets, blobs
}

func runAgentSkillsFixture(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, assets map[string]engine.RunStartedAsset, blobs state.Blobs) (*state.InMemoryLog, *fake.Fake) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk := runAgentWithState(t, def, rs, log, blobs, assets)
	return log, fk
}

func runAgentSkillsStagingFixture(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, assets map[string]engine.RunStartedAsset, blobs state.Blobs) (*state.InMemoryLog, *fake.Fake, *container.Fake, container.Handle) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, be, h := runAgentWithStateAndContainer(t, def, rs, log, blobs, assets)
	return log, fk, be, h
}

func runAgentWithState(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, log *state.InMemoryLog, blobs state.Blobs, assets map[string]engine.RunStartedAsset) *fake.Fake {
	t.Helper()
	fk, _, _ := runAgentWithStateAndContainer(t, def, rs, log, blobs, assets)
	return fk
}

func runAgentWithStateAndContainer(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, log *state.InMemoryLog, blobs state.Blobs, assets map[string]engine.RunStartedAsset) (*fake.Fake, *container.Fake, container.Handle) {
	t.Helper()
	fk, be, h, dispatcher := skillsDispatcherWithContainer(t)
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	return fk, be, h
}

func foldedSkillsSelectedReplayState(t *testing.T, assets map[string]engine.RunStartedAsset, blobs state.Blobs, recorded engine.SkillsSelectedData) (*state.InMemoryLog, *engine.RunState) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(recorded)}); err != nil {
		t.Fatalf("append skills.selected: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	return log, rs
}

func skillsDispatcher(t *testing.T) (*fake.Fake, engine.Dispatcher) {
	t.Helper()
	fk, _, _, dispatcher := skillsDispatcherWithContainer(t)
	return fk, dispatcher
}

func skillsDispatcherWithContainer(t *testing.T) (*fake.Fake, *container.Fake, container.Handle, engine.Dispatcher) {
	t.Helper()
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"ok": true}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	return fk, be, h, &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: &reg,
	}
}

func storeTestSkillAssets(t *testing.T, blobs state.Blobs) map[string]engine.RunStartedAsset {
	t.Helper()
	assets, err := engine.StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
		"skill_assets": {
			ID:           "skill_assets",
			DeclaredPath: "skills",
			IsDir:        true,
			Files: []ir.LoadedAssetFile{
				{Path: "billing/SKILL.md", Bytes: []byte("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n"), Size: int64(len("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n"))},
				{Path: "billing/examples/taxes.md", Bytes: []byte("Review tax rules, invoice line items, and customer balances.\n"), Size: int64(len("Review tax rules, invoice line items, and customer balances.\n"))},
				{Path: "kube/SKILL.md", Bytes: []byte("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n"), Size: int64(len("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n"))},
				{Path: "kube/examples/network.md", Bytes: []byte("Inspect pod DNS, NetworkPolicy, and service routing.\n"), Size: int64(len("Inspect pod DNS, NetworkPolicy, and service routing.\n"))},
			},
		},
	})
	if err != nil {
		t.Fatalf("StoreRunStartedAssets: %v", err)
	}
	return assets
}

func testSkillCorpusDigest(t *testing.T) string {
	t.Helper()
	corpus, err := skillroute.NewCorpus([]skillroute.File{
		{Path: "billing/SKILL.md", Content: []byte("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n")},
		{Path: "billing/examples/taxes.md", Content: []byte("Review tax rules, invoice line items, and customer balances.\n")},
		{Path: "kube/SKILL.md", Content: []byte("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n")},
		{Path: "kube/examples/network.md", Content: []byte("Inspect pod DNS, NetworkPolicy, and service routing.\n")},
	})
	if err != nil {
		t.Fatalf("skillroute.NewCorpus: %v", err)
	}
	return corpus.Digest()
}

func assertNoEventForPath(t *testing.T, log *state.InMemoryLog, eventType, path string) {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type == eventType && ev.Path == path {
			t.Fatalf("found unexpected %s at %q: %+v", eventType, path, ev)
		}
	}
}

func assertHasEventForPath(t *testing.T, log *state.InMemoryLog, eventType, path string) {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type == eventType && ev.Path == path {
			return
		}
	}
	t.Fatalf("missing expected %s at %q", eventType, path)
}
