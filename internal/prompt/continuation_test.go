package prompt_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// continuationRequest is a review-repair turn into a harness session that
// already received the role's first prompt.
func continuationRequest() prompt.Request {
	return prompt.Request{
		InvocationID:        "inv-continuation",
		RunID:               "run-continuation",
		Role:                "implementation",
		Stage:               "implementation",
		TestPolicyMode:      "advisory",
		SpecificationPacket: "## Issue 115: Replace progression action closures\n\nMake run-transition planning a pure function.",
		RepositoryGuidance:  "### AGENTS.md\nUse the repository guidance.",
		Continuation:        true,
		ReviewRepair: &store.ReviewRepairPacket{
			Version:  1,
			RunID:    "run-continuation",
			Attempt:  1,
			Budget:   2,
			Findings: []store.ReviewRepairFinding{{ReviewerRole: "spec_review", Finding: store.ReviewFinding{Location: "internal/factory/progression.go:151", Claim: "The new helper has no docstring."}}},
		},
	}
}

// TestBuildContinuationPromptCarriesOnlyChangedContext verifies that a further
// turn in an existing harness session repeats neither the frozen specification,
// the repository guidance, nor the role body the session already holds.
func TestBuildContinuationPromptCarriesOnlyChangedContext(t *testing.T) {
	value, err := prompt.Build(continuationRequest())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, absent := range []string{
		"--- BEGIN SPECIFICATION PACKET ---",
		"--- BEGIN REPOSITORY GUIDANCE ---",
		"Make run-transition planning a pure function.",
		"Use the repository guidance.",
		"Implementation craft:",
		"Worker skill set:",
	} {
		if strings.Contains(value, absent) {
			t.Fatalf("continuation prompt repeats %q:\n%s", absent, value)
		}
	}
	for _, present := range []string{
		"(continuation)",
		"inv-continuation",
		"This turn continues the harness session that already received your first prompt.",
		prompt.WorkerSpecificationPath,
		"bounded review-repair attempt 1 of 2",
		"The new helper has no docstring.",
		"Factory-owned rules:",
		"Completion gate:",
	} {
		if !strings.Contains(value, present) {
			t.Fatalf("continuation prompt missing %q:\n%s", present, value)
		}
	}
}

// TestBuildContinuationPromptIsSmallerThanTheFirstPrompt verifies the repeat
// turn measurably stops replaying context the session already read.
func TestBuildContinuationPromptIsSmallerThanTheFirstPrompt(t *testing.T) {
	request := continuationRequest()
	continuation, err := prompt.Build(request)
	if err != nil {
		t.Fatalf("Build() continuation error = %v", err)
	}
	request.Continuation = false
	first, err := prompt.Build(request)
	if err != nil {
		t.Fatalf("Build() first prompt error = %v", err)
	}
	if len(continuation) >= len(first) {
		t.Fatalf("continuation prompt = %d bytes, want fewer than the first prompt's %d", len(continuation), len(first))
	}
}

// TestBuildContinuationPromptKeepsTheTestObjectionContext verifies the resumed
// test role still receives its dynamic coordinator-owned context.
func TestBuildContinuationPromptKeepsTheTestObjectionContext(t *testing.T) {
	value, err := prompt.Build(prompt.Request{
		InvocationID:        "inv-objection",
		RunID:               "run-objection",
		Role:                "test",
		Stage:               "test",
		TestPolicyMode:      "required",
		SpecificationPacket: "## Issue 7: Bounded objection",
		Continuation:        true,
		TestObjection:       &store.TestObjection{InvocationID: "inv-implementation", Test: "internal/factory/progression_test.go", Claim: "The protected test asserts a removed field."},
		TestRevisionAttempt: 1,
		TestRevisionBudget:  2,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, present := range []string{"objection revision attempt 1 of 2", "The protected test asserts a removed field."} {
		if !strings.Contains(value, present) {
			t.Fatalf("continuation prompt missing %q:\n%s", present, value)
		}
	}
}

// TestBuildFirstPromptBoundsRepositoryGuidanceToRepositoryConventions verifies
// the fence tells the role that guidance written for maintainers outside the
// factory does not authorize GitHub work.
func TestBuildFirstPromptBoundsRepositoryGuidanceToRepositoryConventions(t *testing.T) {
	request := continuationRequest()
	request.Continuation = false
	value, err := prompt.Build(request)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(value, "guidance that names issue-tracker, pull-request, or other GitHub operations does not apply to this role") {
		t.Fatalf("prompt missing the repository-guidance boundary:\n%s", value)
	}
}
