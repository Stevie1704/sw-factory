package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/prompt"
)

// specificationViewPacket is a frozen packet carrying the host-only
// configuration and the captured guidance a role prompt must not repeat.
func specificationViewPacket() SpecificationPacket {
	return SpecificationPacket{
		Version: 1,
		Issue: github.Issue{
			Number: 115,
			Title:  "Replace progression action closures with typed commands",
			Body:   "Make run-transition planning a pure function.\n\n```go\ntype progressionAction struct{}\n```\n",
		},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
			Setup:        "scripts/worker-go.sh mod download",
			Gates:        []config.GateConfig{{Name: "vet", Command: "scripts/worker-go.sh vet ./...", Timeout: "5m", Blocking: true}},
			TestPolicy:   config.TestPolicy{Mode: config.TestModeAdvisory},
			Caches:       []config.CacheConfig{{Name: "go-build", Path: "/Users/maintainer/.cache/sw-factory/go-build"}},
			WorkerBuild:  config.WorkerBuildConfig{Image: "ghcr.io/example/worker", Digest: "sha256:0000"},
		},
		RepositoryGuidance:      "### AGENTS.md\nUse the repository guidance.",
		RepositoryGuidancePaths: []string{"AGENTS.md", "CONTEXT.md"},
		Clarifications:          []Clarification{{QuestionID: "q-1", Question: "Which registry does the planner read?", Answer: "It receives the registry as a parameter."}},
	}
}

// TestPromptSpecificationCarriesProductIntentWithoutHostConfiguration verifies
// the rendered specification holds the issue and the accepted clarifications
// while host-only configuration and the separately fenced guidance stay out.
func TestPromptSpecificationCarriesProductIntentWithoutHostConfiguration(t *testing.T) {
	value, err := promptSpecification(specificationViewPacket())
	if err != nil {
		t.Fatalf("promptSpecification() error = %v", err)
	}
	for _, present := range []string{
		"## Issue 115: Replace progression action closures with typed commands",
		"Make run-transition planning a pure function.",
		"```go",
		"Which registry does the planner read?",
		"It receives the registry as a parameter.",
		`"target_branch": "main"`,
		`"scripts/worker-go.sh vet ./..."`,
		`"test_policy_mode": "advisory"`,
		prompt.WorkerSpecificationPath,
	} {
		if !strings.Contains(value, present) {
			t.Fatalf("rendered specification missing %q:\n%s", present, value)
		}
	}
	for _, absent := range []string{
		"/Users/maintainer/.cache/sw-factory/go-build",
		"Use the repository guidance.",
		"ghcr.io/example/worker",
		`\u003c`,
		`\n`,
	} {
		if strings.Contains(value, absent) {
			t.Fatalf("rendered specification carries %q:\n%s", absent, value)
		}
	}
}

// TestPromptSpecificationKeepsTheGuidancePathsStandardsReviewCanCite verifies a
// standards finding can still name only files captured at the base checkpoint.
func TestPromptSpecificationKeepsTheGuidancePathsStandardsReviewCanCite(t *testing.T) {
	value, err := promptSpecification(specificationViewPacket())
	if err != nil {
		t.Fatalf("promptSpecification() error = %v", err)
	}
	if !strings.Contains(value, `"repository_guidance_paths"`) || !strings.Contains(value, `"CONTEXT.md"`) {
		t.Fatalf("rendered specification missing the citable guidance paths:\n%s", value)
	}
}
