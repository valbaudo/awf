package ir

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/skillroute"
)

var skillValidationCodes = []string{
	"AWF1040",
	"AWF1041",
	"AWF1042",
	"AWF1043",
	"AWF1044",
	"AWF1045",
	"AWF3010",
	"AWF3011",
}

func validSkillRoutingLD() *LoadedDefinition {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets: map[string]string{"skill_assets": "skills"},
		Skills: map[string]SkillCorpus{
			"skills": {From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName},
		},
		Containers: awf5003Container(),
		Graph: NodeList{
			&AgentStep{
				ID:        "ask",
				Container: "c",
				Uses:      "awf/llm",
				Skills: &StepSkillRouting{
					From:  "skills",
					Query: Template("find repair advice"),
					Limit: 3,
					Into:  "/skills",
				},
			},
		},
	})
	ld.Assets = map[string]LoadedAsset{
		"skill_assets": loadedSkillAsset("skill_assets", true, []LoadedAssetFile{
			{Path: "assist/SKILL.md", Bytes: []byte("# Assist")},
		}),
	}
	return ld
}

func loadedSkillAsset(id string, isDir bool, files []LoadedAssetFile) LoadedAsset {
	return LoadedAsset{ID: id, DeclaredPath: id, IsDir: isDir, Files: files}
}

func TestValidateSkillsValidCorpusAndAgentRoutingNoSkillDiagnostics(t *testing.T) {
	diags := Validate(validSkillRoutingLD())
	for _, code := range skillValidationCodes {
		assertNoErrorCode(t, diags, code)
	}
}

func TestValidateSkillsBadCorpusIDReportsAWF1040(t *testing.T) {
	ld := validSkillRoutingLD()
	ld.Workflow.Skills = map[string]SkillCorpus{
		"bad/id": {From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName},
	}

	assertErrorAt(t, Validate(ld), "AWF1040", "skills.bad/id")
}

func TestValidateSkillsCorpusFromReportsAWF1040OrAWF1041(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   string
		assets map[string]LoadedAsset
		code   string
	}{
		{
			name:   "template",
			from:   "{{ asset.skill_assets }}",
			assets: validSkillRoutingLD().Assets,
			code:   "AWF1040",
		},
		{
			name:   "non asset ref",
			from:   "step.producer.files.skills",
			assets: validSkillRoutingLD().Assets,
			code:   "AWF1040",
		},
		{
			name:   "unknown asset",
			from:   "asset.missing",
			assets: validSkillRoutingLD().Assets,
			code:   "AWF1041",
		},
		{
			name: "file asset",
			from: "asset.file_asset",
			assets: map[string]LoadedAsset{
				"file_asset": loadedSkillAsset("file_asset", false, []LoadedAssetFile{{Path: "SKILL.md", Bytes: []byte("# file")}}),
			},
			code: "AWF1041",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ld := validSkillRoutingLD()
			ld.Workflow.Skills["skills"] = SkillCorpus{From: tc.from, Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName}
			ld.Assets = tc.assets

			assertErrorAt(t, Validate(ld), tc.code, "skills.skills")
		})
	}
}

func TestValidateSkillsUnsupportedLayoutOrRouterReportsAWF1042(t *testing.T) {
	for _, tc := range []struct {
		name   string
		corpus SkillCorpus
	}{
		{
			name:   "layout",
			corpus: SkillCorpus{From: "asset.skill_assets", Layout: "directory", Router: skillroute.RouterName},
		},
		{
			name:   "router",
			corpus: SkillCorpus{From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: "vector"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ld := validSkillRoutingLD()
			ld.Workflow.Skills["skills"] = tc.corpus

			assertErrorAt(t, Validate(ld), "AWF1042", "skills.skills")
		})
	}
}

func TestValidateSkillsLoadedCorpusLayoutReportsAWF3010OrAWF3011(t *testing.T) {
	for _, tc := range []struct {
		name string
		file LoadedAssetFile
		code string
	}{
		{
			name: "root file",
			file: LoadedAssetFile{Path: "README.md", Bytes: []byte("# root")},
			code: "AWF3010",
		},
		{
			name: "missing skill md",
			file: LoadedAssetFile{Path: "assist/README.md", Bytes: []byte("# assist")},
			code: "AWF3010",
		},
		{
			name: "invalid path",
			file: LoadedAssetFile{Path: "assist/../SKILL.md", Bytes: []byte("# bad")},
			code: "AWF3011",
		},
		{
			name: "invalid skill id",
			file: LoadedAssetFile{Path: "1assist/SKILL.md", Bytes: []byte("# bad")},
			code: "AWF3011",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ld := validSkillRoutingLD()
			ld.Assets["skill_assets"] = loadedSkillAsset("skill_assets", true, []LoadedAssetFile{tc.file})

			assertErrorAt(t, Validate(ld), tc.code, "skills.skills")
		})
	}
}

func TestValidateSkillsStepUnknownCorpusReportsAWF1044(t *testing.T) {
	ld := validSkillRoutingLD()
	ld.Workflow.Graph = NodeList{
		&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
			From: "missing", Query: Template("topic"), Limit: 1, Into: "/skills",
		}},
	}

	assertErrorAt(t, Validate(ld), "AWF1044", "ask")
}

func TestValidateSkillsStepRequiresContainerAndNonOverlappingStaging(t *testing.T) {
	for _, tc := range []struct {
		name       string
		container  string
		into       string
		inputFiles map[string]string
	}{
		{
			name:      "missing container",
			container: "",
			into:      "/skills",
		},
		{
			name:      "staging parent of input file",
			container: "c",
			into:      "/skills",
			inputFiles: map[string]string{
				"/skills/context": "asset.skill_assets",
			},
		},
		{
			name:      "staging child of input file",
			container: "c",
			into:      "/skills/context",
			inputFiles: map[string]string{
				"/skills": "asset.skill_assets",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ld := validSkillRoutingLD()
			ld.Workflow.Graph = NodeList{
				&AgentStep{ID: "ask", Container: tc.container, Uses: "awf/llm", Skills: &StepSkillRouting{
					From: "skills", Query: Template("topic"), Limit: 1, Into: tc.into,
				}, InputFiles: tc.inputFiles},
			}

			assertErrorAt(t, Validate(ld), "AWF1045", "ask")
		})
	}
}

func TestValidateSkillsStepShapeReportsAWF1043(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query Template
		limit int
		into  string
	}{
		{name: "empty query", query: Template(" \t\n"), limit: 1, into: "/skills"},
		{name: "zero limit", query: Template("topic"), limit: 0, into: "/skills"},
		{name: "negative limit", query: Template("topic"), limit: -1, into: "/skills"},
		{name: "limit above max", query: Template("topic"), limit: skillroute.MaxSelectionLimit + 1, into: "/skills"},
		{name: "relative into", query: Template("topic"), limit: 1, into: "skills"},
		{name: "non clean into", query: Template("topic"), limit: 1, into: "/skills/../skills"},
		{name: "root into", query: Template("topic"), limit: 1, into: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ld := validSkillRoutingLD()
			ld.Workflow.Graph = NodeList{
				&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
					From: "skills", Query: tc.query, Limit: tc.limit, Into: tc.into,
				}},
			}

			assertErrorAt(t, Validate(ld), "AWF1043", "ask")
		})
	}
}

func TestValidateSkillRoutingQueryRefs(t *testing.T) {
	t.Run("bad step field reports AWF3001", func(t *testing.T) {
		schema := &JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
			"additionalProperties": false,
		}
		ld := validSkillRoutingLD()
		ld.Workflow.Graph = NodeList{
			&CodeStep{ID: "producer", Container: "c", Run: "echo ok", OutputSchema: schema},
			&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
				From: "skills", Query: Template("use {{ step.producer.missing }}"), Limit: 1, Into: "/skills",
			}},
		}

		assertErrorAt(t, Validate(ld), "AWF3001", "ask.skills.query")
	})

	t.Run("evaluate outside gate reports AWF5001", func(t *testing.T) {
		ld := validSkillRoutingLD()
		ld.Workflow.Graph = NodeList{
			&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
				From: "skills", Query: Template("use {{ evaluate.feedback }}"), Limit: 1, Into: "/skills",
			}},
		}

		assertErrorAt(t, Validate(ld), "AWF5001", "ask.skills.query")
	})

	t.Run("evaluate inside gate generate is allowed", func(t *testing.T) {
		schema := &JSONSchema{
			"type":                 "object",
			"properties":           map[string]any{"feedback": map[string]any{"type": "string"}},
			"additionalProperties": false,
		}
		ld := validSkillRoutingLD()
		ld.Workflow.Graph = NodeList{
			&Gate{
				Generate: NodeList{
					&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
						From: "skills", Query: Template("use {{ evaluate.feedback }}"), Limit: 1, Into: "/skills",
					}},
				},
				Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "echo ok", OutputSchema: schema}},
				Until:       Expr("{{ evaluate.feedback != '' }}"),
				MaxAttempts: 2,
			},
		}

		assertNoErrorCode(t, Validate(ld), "AWF5001")
	})

	t.Run("oversized query reports AWF1016", func(t *testing.T) {
		ld := validSkillRoutingLD()
		ld.Workflow.Graph = NodeList{
			&AgentStep{ID: "ask", Container: "c", Uses: "awf/llm", Skills: &StepSkillRouting{
				From: "skills", Query: Template(strings.Repeat("a", maxExpressionBytes+1)), Limit: 1, Into: "/skills",
			}},
		}

		assertErrorAt(t, Validate(ld), "AWF1016", "ask.skills.query")
	})
}
