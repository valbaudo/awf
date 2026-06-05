package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// p6aRuntimeImageWorkflow — a map whose per-element image is runtime-resolved
// from the worklist (P6a). version_lab is resources-only (its image arrives per
// element). min_success 0.5 lets the unavailable-image sub-test tolerate a
// per-item boot failure.
const p6aRuntimeImageWorkflow = `workflow: conformance-p6a-runtime-image
version: 1
input:
  type: object
  additionalProperties: false
  required: [items]
  properties:
    items:
      type: array
      items:
        type: object
        additionalProperties: false
        required: [image, name]
        properties:
          image: { type: string }
          name:  { type: string }
containers:
  version_lab:
    resources: { cpu: "1", mem: 1Gi }
graph:
  - map:
      over: "{{ input.items }}"
      as: v
      container: version_lab
      image: "{{ v.image }}"
      concurrency: 2
      min_success: 0.5
      body:
        - id: probe
          container: version_lab
          run: "./probe.sh {{ v.name }}"
          retry: { attempts: 1 }
`

// realistic published-image refs + their resolved content digests — modeling
// "tag in the worklist, digest captured at boot" so the conformance contract is
// forward-compatible with the docker follow-up's RepoDigests capture.
func p6aDigest(c byte) string { return "registry.example.com/app@sha256:" + strings.Repeat(string(c), 64) }

const (
	p6aImgA = "registry.example.com/app:1.2.3"
	p6aImgB = "registry.example.com/app:1.2.4"
	p6aImgC = "registry.example.com/app:1.2.5"
	p6aGone = "registry.example.com/app:gone"
)

// preProgramFakeP6a wraps factory so every *container.Fake is pre-loaded with
// exec results, image→digest mappings, and unavailable images. Non-fake
// backends pass through (this bucket is fake-only).
func preProgramFakeP6a(t *testing.T, factory BackendFactory, execs []execProgram, digests map[string]string, unavailable []string) BackendFactory {
	t.Helper()
	return func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		for _, p := range execs {
			fake.ProgramExec(p.cmd, p.res, nil)
		}
		for img, dig := range digests {
			fake.ProgramImageDigest(img, dig)
		}
		for _, img := range unavailable {
			fake.FailCreateForImage(img)
		}
		return fake
	}
}

func p6aItems(pairs ...[2]string) map[string]any {
	items := make([]any, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, map[string]any{"image": p[0], "name": p[1]})
	}
	return map[string]any{"items": items}
}

type p6aItem struct {
	status, digest, reason string
}

func p6aMapItems(t *testing.T, h *harness) (map[int]p6aItem, int) {
	t.Helper()
	out := map[int]p6aItem{}
	count := 0
	for _, e := range mustFoldEvents(t, h) {
		if e.Type == engine.EventMapItem {
			var d engine.MapItemData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal map.item: %v", err)
			}
			out[d.N] = p6aItem{status: d.Status, digest: d.ImageDigest, reason: d.Reason}
			count++
		}
	}
	return out, count
}

func testP6a(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("captures_digest_on_first_boot", func(t *testing.T) { testP6aCaptureOnFirstBoot(t, factory) })
	t.Run("resume_replays_committed_items_with_digest", func(t *testing.T) { testP6aResumeReplays(t, factory) })
	t.Run("unavailable_image_is_item_failed_with_reason", func(t *testing.T) { testP6aUnavailableItemFailed(t, factory) })
}

// (1) The runtime-resolved digest is captured into each element's map.item
// commit at first boot.
func testP6aCaptureOnFirstBoot(t *testing.T, factory BackendFactory) {
	t.Helper()
	f := preProgramFakeP6a(t, factory,
		[]execProgram{
			{cmd: "./probe.sh n0", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./probe.sh n1", res: container.ExecResult{ExitCode: 0}},
		},
		map[string]string{p6aImgA: p6aDigest('a'), p6aImgB: p6aDigest('b')},
		nil)
	h := newHarnessWithInput(t, f, p6aRuntimeImageWorkflow, p6aItems([2]string{p6aImgA, "n0"}, [2]string{p6aImgB, "n1"}))
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	items, count := p6aMapItems(t, h)
	if count != 2 {
		t.Fatalf("map.item count = %d, want 2", count)
	}
	if items[0].digest != p6aDigest('a') || items[1].digest != p6aDigest('b') {
		t.Errorf("captured digests = {0:%q 1:%q}, want a/b digests", items[0].digest, items[1].digest)
	}
}

// (2) On resume, committed elements REPLAY from the journal (no body re-exec,
// no container re-create) against a BARE fake — and their captured digest
// SURVIVES the re-fold into run-state. Proves a committed runtime-image element
// never re-resolves its reference, and the digest is durable run state.
//
// NOTE (limitation, honest): a map runs all items to completion before its node
// commits, and a committed item (passed OR failed) is replayed-as-skipped on
// resume (engine/map.go). So there is no uncommitted "frontier" item to re-render
// for a map under this harness — a true mid-map process-crash frontier test is
// not expressible here (same limitation the existing map resume bucket has). The
// durable assertions below (replay + digest-survives-refold) are the meaningful
// resume guarantees for P6a.
func testP6aResumeReplays(t *testing.T, factory BackendFactory) {
	t.Helper()
	f := preProgramFakeP6a(t, factory,
		[]execProgram{
			{cmd: "./probe.sh n0", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./probe.sh n1", res: container.ExecResult{ExitCode: 0}},
		},
		map[string]string{p6aImgA: p6aDigest('a'), p6aImgB: p6aDigest('b')},
		nil)
	h := newHarnessWithInput(t, f, p6aRuntimeImageWorkflow, p6aItems([2]string{p6aImgA, "n0"}, [2]string{p6aImgB, "n1"}))
	oc1, err1 := h.runWorkflow(t)
	if err1 != nil {
		t.Fatalf("round-1: %v", err1)
	}
	if oc1 != engine.OutcomeOK {
		t.Fatalf("round-1 outcome = %q, want ok", oc1)
	}
	_, pre := p6aMapItems(t, h)
	if pre != 2 {
		t.Fatalf("round-1 map.item count = %d, want 2", pre)
	}

	// Round 2: BARE fake (no programmed exec, no digest table). If a committed
	// element re-executed or re-resolved its image, the bare fake would error or
	// duplicate a map.item. Neither must happen.
	h.factory = factory
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("round-2 resume: %v (committed elements must replay, not re-execute)", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("round-2 outcome = %q, want ok", oc2)
	}
	_, post := p6aMapItems(t, h)
	if post != 2 {
		t.Errorf("post-resume map.item count = %d, want 2 (committed elements replay, not duplicate)", post)
	}
	// The captured digest survives the re-fold into run-state (durability invariant).
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("re-fold: %v", err)
	}
	byN := map[int]string{}
	for _, mr := range rs.LookupMapItems("map[0]") {
		byN[mr.N] = mr.ImageDigest
	}
	if byN[0] != p6aDigest('a') || byN[1] != p6aDigest('b') {
		t.Errorf("post-resume run-state digests = {0:%q 1:%q}, want a/b digests (digest must survive the re-fold)", byN[0], byN[1])
	}
}

// (3) An unavailable runtime image fails THAT element only (item_failed +
// reason image_unavailable, counted against min_success) — the map is not
// hard-aborted.
func testP6aUnavailableItemFailed(t *testing.T, factory BackendFactory) {
	t.Helper()
	f := preProgramFakeP6a(t, factory,
		[]execProgram{
			{cmd: "./probe.sh n0", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./probe.sh n2", res: container.ExecResult{ExitCode: 0}},
		},
		map[string]string{p6aImgA: p6aDigest('a'), p6aImgC: p6aDigest('c')},
		[]string{p6aGone})
	h := newHarnessWithInput(t, f, p6aRuntimeImageWorkflow,
		p6aItems([2]string{p6aImgA, "n0"}, [2]string{p6aGone, "n1"}, [2]string{p6aImgC, "n2"}))
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok (2/3 boot meets min_success 0.5)", oc)
	}
	items, count := p6aMapItems(t, h)
	if count != 3 {
		t.Fatalf("map.item count = %d, want 3", count)
	}
	if items[1].status != engine.ItemFailed || items[1].reason != "image_unavailable" {
		t.Errorf("item N=1 = {status:%q reason:%q}, want {item_failed image_unavailable}", items[1].status, items[1].reason)
	}
	if items[0].status != engine.ItemPassed || items[2].status != engine.ItemPassed {
		t.Errorf("items 0/2 statuses = %q/%q, want item_passed", items[0].status, items[2].status)
	}
}
