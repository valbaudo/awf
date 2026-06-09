package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"reflect"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

var (
	errSkillsSelectedMetadataMismatch = errors.New("engine: skills.selected metadata mismatch")
	errSkillsSelectedInvalidSelection = errors.New("engine: invalid skills.selected selection")
)

func selectAgentStepSkills(
	as *ir.AgentStep,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	log state.Log,
	blobs state.Blobs,
	scope *Scope,
) (SkillsSelectedData, *skillroute.Corpus, Outcome, error) {
	corpus, err := buildSkillCorpus(as.Skills.From, wf, runstate.Assets, blobs)
	if err != nil {
		if errors.Is(err, errArtifactFetch) {
			return SkillsSelectedData{}, nil, "", err
		}
		return SkillsSelectedData{}, nil, OutcomePermanentFailure, err
	}
	current := skillsSelectedMetadata(as.Skills.From, corpus)

	if recorded, ok := runstate.LookupSelectedSkills(path); ok {
		if !sameSkillsSelectedMetadata(recorded, current) {
			return SkillsSelectedData{}, nil, "", fmt.Errorf("%w at path %q", errSkillsSelectedMetadataMismatch, path)
		}
		if err := validateRecordedSelectedSkills(recorded.Selected, corpus); err != nil {
			return SkillsSelectedData{}, nil, "", err
		}
		return recorded, corpus, OutcomeOK, nil
	}

	renderedQuery, err := template.Substitute(string(as.Skills.Query), scope)
	if err != nil {
		return SkillsSelectedData{}, nil, OutcomePermanentFailure, err
	}
	selections := corpus.Route(renderedQuery, as.Skills.Limit)
	if len(selections) == 0 {
		return SkillsSelectedData{}, nil, OutcomePermanentFailure, fmt.Errorf("engine: no skills matched library %q query", as.Skills.From)
	}
	data := current
	data.Selected = make([]SelectedSkill, 0, len(selections))
	for _, sel := range selections {
		data.Selected = append(data.Selected, SelectedSkill{ID: sel.ID, Score: sel.Score})
	}
	if err := appendSkillsSelected(log, path, data); err != nil {
		return SkillsSelectedData{}, nil, "", err
	}
	runstate.RecordSelectedSkills(path, data)
	return data, corpus, OutcomeOK, nil
}

func buildSkillCorpus(id string, wf *ir.Workflow, assets map[string]RunStartedAsset, blobs state.Blobs) (*skillroute.Corpus, error) {
	corpusDef, ok := wf.Skills[id]
	if !ok {
		return nil, fmt.Errorf("skill library %q is not declared", id)
	}
	assetID, ok := template.ParseAssetRef(corpusDef.From)
	if !ok {
		return nil, fmt.Errorf("skill library %q from %q must be an asset ref", id, corpusDef.From)
	}
	asset, ok := assets[assetID]
	if !ok {
		return nil, fmt.Errorf("%w: skill library %q asset %q was not recorded in run.started", errArtifactFetch, id, assetID)
	}
	if !asset.IsDir {
		return nil, fmt.Errorf("%w: skill library %q asset %q must be a directory", errArtifactFetch, id, assetID)
	}
	files := make([]skillroute.File, 0, len(asset.Files))
	for _, f := range asset.Files {
		b, err := readRunStartedAssetFile(blobs, f)
		if err != nil {
			return nil, err
		}
		files = append(files, skillroute.File{
			Path:    f.Path,
			Content: b,
			Size:    f.Size,
			SHA256:  f.SHA256,
		})
	}
	corpus, err := skillroute.NewCorpus(id, files)
	if err != nil {
		return nil, err
	}
	return corpus, nil
}

func skillsSelectedMetadata(id string, corpus *skillroute.Corpus) SkillsSelectedData {
	return SkillsSelectedData{
		Library:       id,
		LibraryDigest: corpus.Digest(),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
	}
}

func sameSkillsSelectedMetadata(a, b SkillsSelectedData) bool {
	return a.Library == b.Library &&
		a.LibraryDigest == b.LibraryDigest &&
		a.Router == b.Router &&
		a.RouterVersion == b.RouterVersion &&
		reflect.DeepEqual(a.RouterParams, b.RouterParams)
}

func validateSkillsSelectedEventLocal(data SkillsSelectedData) error {
	if data.Library == "" {
		return fmt.Errorf("%w: library must be non-empty", errSkillsSelectedInvalidSelection)
	}
	if data.LibraryDigest == "" {
		return fmt.Errorf("%w: library_digest must be non-empty", errSkillsSelectedInvalidSelection)
	}
	if data.Router == "" {
		return fmt.Errorf("%w: router must be non-empty", errSkillsSelectedInvalidSelection)
	}
	if data.RouterVersion == "" {
		return fmt.Errorf("%w: router_version must be non-empty", errSkillsSelectedInvalidSelection)
	}
	return validateSelectedSkillsEventLocal(data.Selected)
}

func validateSelectedSkillsEventLocal(selected []SelectedSkill) error {
	if len(selected) == 0 {
		return fmt.Errorf("%w: selected must be non-empty", errSkillsSelectedInvalidSelection)
	}
	seen := map[string]bool{}
	for _, sel := range selected {
		if sel.ID == "" {
			return fmt.Errorf("%w: selected id must be non-empty", errSkillsSelectedInvalidSelection)
		}
		if seen[sel.ID] {
			return fmt.Errorf("%w: selected id %q is duplicated", errSkillsSelectedInvalidSelection, sel.ID)
		}
		seen[sel.ID] = true
		if math.IsNaN(sel.Score) || math.IsInf(sel.Score, 0) {
			return fmt.Errorf("%w: selected id %q has non-finite score", errSkillsSelectedInvalidSelection, sel.ID)
		}
		if sel.Score <= 0 {
			return fmt.Errorf("%w: selected id %q has non-positive score", errSkillsSelectedInvalidSelection, sel.ID)
		}
	}
	return nil
}

func validateRecordedSelectedSkills(selected []SelectedSkill, corpus *skillroute.Corpus) error {
	if err := validateSelectedSkillsEventLocal(selected); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, id := range corpus.SkillIDs() {
		known[id] = true
	}
	for _, sel := range selected {
		if !known[sel.ID] {
			return fmt.Errorf("%w: selected id %q is not in the current corpus", errSkillsSelectedInvalidSelection, sel.ID)
		}
	}
	return nil
}

func appendSkillsSelected(log state.Log, path string, data SkillsSelectedData) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("engine: marshal skills.selected: %w", err)
	}
	if err := log.Append(state.Event{Type: EventSkillsSelected, Path: path, Data: raw}); err != nil {
		return fmt.Errorf("engine: append skills.selected at path %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return fmt.Errorf("engine: sync after skills.selected at path %q: %w", path, err)
	}
	return nil
}

func resolveSelectedSkillFiles(selection SkillsSelectedData, corpus *skillroute.Corpus, into string) ([]resolvedInputFile, error) {
	out := []resolvedInputFile{}
	for _, selected := range selection.Selected {
		files, ok := corpus.StageFiles(selected.ID, into)
		if !ok {
			return nil, fmt.Errorf("%w: selected id %q is not in the current corpus", errSkillsSelectedInvalidSelection, selected.ID)
		}
		source := fmt.Sprintf("skills.%s.%s", selection.Library, selected.ID)
		for _, f := range files {
			if !path.IsAbs(f.Path) || f.Path != path.Clean(f.Path) {
				return nil, fmt.Errorf("skills[%s]: staged destination %q must be an absolute, clean path", selected.ID, f.Path)
			}
			out = append(out, resolvedInputFile{
				file:   container.InputFile{Path: f.Path, Content: f.Content},
				source: source,
			})
		}
	}
	return out, nil
}

func cloneSkillsSelectedData(in SkillsSelectedData) SkillsSelectedData {
	out := in
	if in.RouterParams != nil {
		out.RouterParams = make(map[string]float64, len(in.RouterParams))
		for k, v := range in.RouterParams {
			out.RouterParams[k] = v
		}
	}
	if in.Selected != nil {
		out.Selected = append([]SelectedSkill(nil), in.Selected...)
	}
	return out
}
