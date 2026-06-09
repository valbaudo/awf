package ir

import (
	"fmt"
	"path"
	"strings"

	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/template"
)

func validateSkills(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	validateSkillCorpora(ld, c)
	validateAgentSkillRouting(ld.Workflow, c)
}

func validateSkillCorpora(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow
	for id, corpus := range wf.Skills {
		corpusPath := "skills." + id
		checkSkillCorpusID(id, corpusPath, c)
		assetID, ok := template.ParseAssetRef(corpus.From)
		if !ok {
			c.errf(corpusPath, "AWF1040", fmt.Sprintf("%s: from=%q must be a static asset.<id> reference", catalog["AWF1040"], corpus.From))
		}
		if corpus.Layout != "skill_dirs" || corpus.Router != skillroute.RouterName {
			c.errf(corpusPath, "AWF1042", fmt.Sprintf("%s: layout=%q router=%q", catalog["AWF1042"], corpus.Layout, corpus.Router))
		}
		if !ok {
			continue
		}
		asset, exists := ld.Assets[assetID]
		if !exists || !asset.IsDir {
			c.errf(corpusPath, "AWF1041", fmt.Sprintf("%s: %s", catalog["AWF1041"], corpus.From))
			continue
		}
		validateLoadedSkillCorpus(corpusPath, asset, c)
	}
}

func checkSkillCorpusID(id, corpusPath string, c *collector) {
	if !stepIDPattern.MatchString(id) {
		c.errf(corpusPath, "AWF1040", fmt.Sprintf("%s: id=%q (must match %s)", catalog["AWF1040"], id, stepIDPattern))
	}
	if reservedStepIDTokens[id] {
		c.errf(corpusPath, "AWF1040", fmt.Sprintf("%s: id=%q collides with reserved control keyword", catalog["AWF1040"], id))
	}
}

func validateLoadedSkillCorpus(corpusPath string, asset LoadedAsset, c *collector) {
	files := make([]skillroute.File, 0, len(asset.Files))
	for _, f := range asset.Files {
		files = append(files, skillroute.File{
			Path:    f.Path,
			Content: f.Bytes,
			Size:    f.Size,
			SHA256:  f.SHA256,
		})
	}
	for _, issue := range skillroute.ValidateFiles(files) {
		switch issue.Kind {
		case skillroute.IssueRootFile, skillroute.IssueMissingSkillMD:
			c.errf(corpusPath, "AWF3010", fmt.Sprintf("%s: %s", catalog["AWF3010"], issue.Message))
		case skillroute.IssueInvalidPath, skillroute.IssueInvalidSkillID:
			c.errf(corpusPath, "AWF3011", fmt.Sprintf("%s: %s", catalog["AWF3011"], issue.Message))
		}
	}
}

func validateAgentSkillRouting(wf *Workflow, c *collector) {
	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		step, ok := n.(*AgentStep)
		if !ok || step.Skills == nil {
			return
		}
		routing := step.Skills
		if _, ok := wf.Skills[routing.From]; !ok {
			c.errf(nodePath, "AWF1044", fmt.Sprintf("%s: %q", catalog["AWF1044"], routing.From))
		}
		if strings.TrimSpace(string(routing.Query)) == "" {
			c.errf(nodePath, "AWF1043", fmt.Sprintf("%s: query must be non-empty", catalog["AWF1043"]))
		}
		if routing.Limit <= 0 || routing.Limit > 64 {
			c.errf(nodePath, "AWF1043", fmt.Sprintf("%s: limit must be between 1 and 64", catalog["AWF1043"]))
		}
		intoOK := validateSkillRoutingInto(nodePath, routing.Into, c)
		if strings.TrimSpace(step.Container) == "" {
			c.errf(nodePath, "AWF1045", fmt.Sprintf("%s: container is required when skills are staged", catalog["AWF1045"]))
		}
		if intoOK {
			validateSkillRoutingInputFileOverlap(nodePath, routing.Into, step.InputFiles, c)
		}
	})
}

func validateSkillRoutingInto(nodePath, into string, c *collector) bool {
	if !path.IsAbs(into) || into != path.Clean(into) || into == "/" {
		c.errf(nodePath, "AWF1043", fmt.Sprintf("%s: into=%q must be an absolute, clean, non-root path", catalog["AWF1043"], into))
		return false
	}
	return true
}

func validateSkillRoutingInputFileOverlap(nodePath, into string, inputFiles map[string]string, c *collector) {
	for dst := range inputFiles {
		if !path.IsAbs(dst) || dst != path.Clean(dst) {
			continue
		}
		if dst == into || inputFileDestinationAncestor(dst, into) || inputFileDestinationAncestor(into, dst) {
			c.errf(nodePath, "AWF1045", fmt.Sprintf("%s: skills.into %s overlaps input_files destination %s", catalog["AWF1045"], into, dst))
		}
	}
}
