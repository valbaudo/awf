package engine

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
)

// TestBuildSkillCorpusQualifiesModuleKey reproduces P3: in a sub-workflow the
// skill-library asset is recorded under QualifiedAssetKey(moduleID, id); the
// bare-id lookup misses. Root (moduleID="") must still resolve via the bare key.
func TestBuildSkillCorpusQualifiesModuleKey(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	wf := &ir.Workflow{
		Skills: map[string]ir.SkillCorpus{
			"awf": {From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName},
		},
	}
	mkAssets := func(key string) map[string]RunStartedAsset {
		a, err := StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
			key: {ID: "skill_assets", DeclaredPath: "skills", IsDir: true, Files: []ir.LoadedAssetFile{
				{Path: "billing/SKILL.md", Bytes: []byte("# Billing\nReconcile invoices.\n")},
			}},
		})
		if err != nil {
			t.Fatalf("StoreRunStartedAssets(%q): %v", key, err)
		}
		return a
	}

	// Root: bare key, moduleID "" → resolves.
	if _, err := buildSkillCorpus("awf", wf, "", mkAssets("skill_assets"), blobs); err != nil {
		t.Fatalf("root buildSkillCorpus: unexpected err %v", err)
	}
	// Sub-workflow: assets keyed "child/skill_assets", moduleID "child" → must resolve.
	if _, err := buildSkillCorpus("awf", wf, "child", mkAssets("child/skill_assets"), blobs); err != nil {
		t.Fatalf("sub-workflow buildSkillCorpus: got err %v (errArtifactFetch=%v); want nil", err, errors.Is(err, errArtifactFetch))
	}
}
