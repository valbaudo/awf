package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func testSkills(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("skills bucket records fake CopyTo calls; fake-only")
	}
	t.Run("selected_skill_staged_from_run_started_snapshot", func(t *testing.T) {
		testSkillsSelectedSkillStagedFromRunStartedSnapshot(t, factory)
	})
	t.Run("resume_replays_selected_skill_and_re_stages", func(t *testing.T) {
		testSkillsResumeReplaysSelectedSkillAndReStages(t, factory)
	})
}

func testSkillsSelectedSkillStagedFromRunStartedSnapshot(t *testing.T, factory BackendFactory) {
	t.Helper()

	var spy *assetCopyToSpy
	var h *harness
	h = newSkillsHarness(t, func() container.Backend {
		// runOrResume snapshots assets into run.started before constructing the
		// backend, so this mutation must not affect selected skill staging.
		poisonSkillAssets(t, h)
		b := factory()
		fakeBackend, ok := b.(*container.Fake)
		if !ok {
			t.Fatalf("skills bucket factory returned %T, want *container.Fake", b)
		}
		spy = newAssetCopyToSpy(fakeBackend)
		return spy
	}, func(fk *fake.Fake) {
		fk.Script(0, fake.Result{Output: map[string]any{"ok": true}})
	})
	writeSkillAssets(t, h)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	if spy == nil {
		t.Fatal("workflow did not create a fake backend")
	}

	events := mustFoldEvents(t, h)
	if got := countEventsForPath(events, engine.EventSkillsSelected, "hunt"); got != 1 {
		t.Fatalf("skills.selected event count for hunt = %d, want 1", got)
	}

	rs, err := engine.Fold(events, h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	selected, ok := rs.SelectedSkills["hunt"]
	if !ok {
		t.Fatal("RunState.SelectedSkills[hunt] missing")
	}
	if got := len(selected.Selected); got != 1 {
		t.Fatalf("selected skill count = %d, want 1: %#v", got, selected.Selected)
	}
	if got := selected.Selected[0].ID; got != "kube" {
		t.Fatalf("selected skill ID = %q, want kube", got)
	}

	want := map[string]string{
		"/work/.awf/skills/kube/SKILL.md":         kubeSkillMD,
		"/work/.awf/skills/kube/examples/pod.txt": kubePodExample,
	}
	if got := spy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("staged files = %#v, want %#v", got, want)
	}
	if _, ok := spy.stagedByPath()["/work/.awf/skills/billing/SKILL.md"]; ok {
		t.Fatal("staged unselected billing skill")
	}
}

func testSkillsResumeReplaysSelectedSkillAndReStages(t *testing.T, factory BackendFactory) {
	t.Helper()

	var runSpy, resumeSpy *assetCopyToSpy
	var h *harness
	h = newSkillsHarness(t, func() container.Backend {
		b := factory()
		fakeBackend, ok := b.(*container.Fake)
		if !ok {
			t.Fatalf("skills bucket factory returned %T, want *container.Fake", b)
		}
		spy := newAssetCopyToSpy(fakeBackend)
		if runSpy == nil {
			runSpy = spy
		} else {
			// On resume, the log is folded before this second backend is created;
			// replay must stage the originally recorded skill bytes.
			poisonSkillAssets(t, h)
			resumeSpy = spy
		}
		return spy
	}, func(fk *fake.Fake) {
		// Invocation 0 is intentionally unscripted. agent/fake surfaces that as a
		// retryable launch failure after skill selection and staging. The resume
		// invocation consumes index 1 and succeeds.
		fk.Script(1, fake.Result{Output: map[string]any{"ok": true}})
	})
	writeSkillAssets(t, h)

	oc, err := h.runWorkflow(t)
	if oc == "" {
		t.Fatalf("first run produced no outcome (err=%v)", err)
	}
	if oc == engine.OutcomeOK {
		t.Fatal("first run unexpectedly ok; fake agent invocation 0 should be unscripted")
	}
	events := mustFoldEvents(t, h)
	if got := countEventsForPath(events, engine.EventSkillsSelected, "hunt"); got != 1 {
		t.Fatalf("first-run skills.selected event count for hunt = %d, want 1", got)
	}
	rs, err := engine.Fold(events, h.blobs)
	if err != nil {
		t.Fatalf("Fold after failed run: %v", err)
	}
	if _, ok := rs.Completed["hunt"]; ok {
		t.Fatal("hunt committed in failed run; resume would skip the re-stage proof")
	}
	if runSpy == nil {
		t.Fatal("first run did not create a fake backend")
	}

	oc2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", oc2)
	}
	if resumeSpy == nil {
		t.Fatal("resume did not create a second fake backend")
	}

	after := mustFoldEvents(t, h)
	if got := countEventsForPath(after, engine.EventSkillsSelected, "hunt"); got != 1 {
		t.Fatalf("skills.selected event count after resume = %d, want 1", got)
	}
	rs2, err := engine.Fold(after, h.blobs)
	if err != nil {
		t.Fatalf("Fold after resume: %v", err)
	}
	if _, ok := rs2.Completed["hunt"]; !ok {
		t.Fatal("hunt not committed after resume")
	}

	want := map[string]string{
		"/work/.awf/skills/kube/SKILL.md":         kubeSkillMD,
		"/work/.awf/skills/kube/examples/pod.txt": kubePodExample,
	}
	if got := runSpy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first-run staged files = %#v, want %#v", got, want)
	}
	if got := resumeSpy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resume staged files = %#v, want %#v", got, want)
	}
}

func newSkillsHarness(t *testing.T, factory BackendFactory, script func(*fake.Fake)) *harness {
	t.Helper()
	register := func(reg *agent.Registry) {
		fk := fake.New("test/skills")
		script(fk)
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register fake skill agent: %v", err)
		}
	}
	return newHarnessWithAgentRegistry(t, factory, skillsWorkflow, register)
}

func writeSkillAssets(t *testing.T, h *harness) {
	t.Helper()
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "kube", "SKILL.md"), []byte(kubeSkillMD))
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "kube", "examples", "pod.txt"), []byte(kubePodExample))
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "billing", "SKILL.md"), []byte(billingSkillMD))
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "billing", "examples", "invoice.txt"), []byte(billingInvoiceExample))
}

func poisonSkillAssets(t *testing.T, h *harness) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(h.baseDir, "skills", "kube")); err != nil {
		t.Fatalf("remove live kube skill: %v", err)
	}
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "billing", "SKILL.md"), []byte(poisonBillingSkillMD))
	writeAssetFile(t, filepath.Join(h.baseDir, "skills", "billing", "examples", "invoice.txt"), []byte(poisonBillingInvoiceExample))
}

func countEventsForPath(events []state.Event, eventType, path string) int {
	count := 0
	for _, e := range events {
		if e.Type == eventType && e.Path == path {
			count++
		}
	}
	return count
}

var skillsWorkflow = fmt.Sprintf(`workflow: conformance-skills
version: 1
assets:
  skill_assets: skills
skills:
  awf:
    from: asset.skill_assets
    layout: skill_dirs
    router: bm25
containers:
  lab:
    image: %s
graph:
  - id: hunt
    container: lab
    uses: test/skills
    with:
      prompt: diagnose incident
    skills:
      from: awf
      query: "kubernetes pod network outage"
      limit: 1
      into: /work/.awf/skills
    retry: { attempts: 1 }
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok:
          type: boolean
`, fakeImageDigest)

const kubeSkillMD = `# Kubernetes Pod Network Outage

Use this skill for kubernetes pod network outage triage. Diagnose pod DNS,
NetworkPolicy, kube-proxy, CNI, service routing, cluster network partitions,
and pod-to-pod connectivity failures.
`

const kubePodExample = `pod network outage checklist:
- inspect kubernetes pod DNS resolution
- inspect CNI routes and NetworkPolicy denies
- compare service endpoints and kube-proxy rules
`

const billingSkillMD = `# Invoice Billing Reconciliation

Use this skill for invoices, tax calculations, payment collection, revenue
recognition, account credits, subscriptions, and billing ledger reconciliation.
`

const billingInvoiceExample = `invoice reconciliation checklist:
- compare customer account credits
- inspect tax rate changes
- verify payment processor settlement batches
`

const poisonBillingSkillMD = `# Poisoned Billing Skill

This live-only poison says kubernetes pod network outage many times:
kubernetes kubernetes pod pod network network outage outage CNI kube-proxy DNS.
If routing reads live files instead of the run-start snapshot, billing wins.
`

const poisonBillingInvoiceExample = `poison staged bytes:
- kubernetes pod network outage
- this content must never be copied from the live tree
`
