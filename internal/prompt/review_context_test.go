package prompt_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// reviewRequestWithDiff builds a specification-review prompt request whose diff
// is larger than every other part of the prompt combined.
func reviewRequestWithDiff(diff string) prompt.Request {
	return prompt.Request{
		InvocationID:        "inv-review",
		RunID:               "run-review",
		Role:                "spec_review",
		Stage:               "review",
		SpecificationPacket: "## Issue 118: Remove the compatibility projections",
		RepositoryGuidance:  "### AGENTS.md\nUse the repository guidance.",
		ReviewContext: &prompt.ReviewContext{
			CheckpointSHA: "e7934d8d93b45db0e2cb89b5781fadb368ee8b58",
			CurrentDiff:   diff,
			RelevantLogs:  []prompt.ReviewLog{{Source: "gate:test", Detail: "passed at the exact checkpoint"}},
			PriorFindings: []store.ReviewFinding{{Location: "internal/store/store.go:1810", Claim: "the projection is still read"}},
		},
	}
}

// TestBuildReviewPromptPointsAtTheMountedDiff verifies the prompt keeps the
// bounded review observations and names where the exact diff is, rather than
// carrying a second copy of content already mounted with the packet.
func TestBuildReviewPromptPointsAtTheMountedDiff(t *testing.T) {
	diff := "diff --git a/internal/store/store.go b/internal/store/store.go\n" + strings.Repeat("-\tremoved line\n", 8000)
	value, err := prompt.Build(reviewRequestWithDiff(diff))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if strings.Contains(value, "removed line") {
		t.Fatalf("review prompt repeats the mounted diff:\n%s", value[:2000])
	}
	for _, present := range []string{
		"e7934d8d93b45db0e2cb89b5781fadb368ee8b58",
		"passed at the exact checkpoint",
		"the projection is still read",
		fmt.Sprintf("is %d bytes", len(diff)),
		prompt.WorkerSpecificationPath,
		"review_context.current_diff",
	} {
		if !strings.Contains(value, present) {
			t.Fatalf("review prompt missing %q:\n%s", present, value)
		}
	}
}

// TestBuildReviewPromptSizeDoesNotFollowTheDiff verifies a reviewed change can
// grow without the prompt growing with it, which is what made a large
// checkpoint impossible to launch.
func TestBuildReviewPromptSizeDoesNotFollowTheDiff(t *testing.T) {
	small, err := prompt.Build(reviewRequestWithDiff("diff --git a/a b/a\n-one line\n"))
	if err != nil {
		t.Fatalf("Build() small diff error = %v", err)
	}
	large, err := prompt.Build(reviewRequestWithDiff(strings.Repeat("-\tremoved line\n", 40000)))
	if err != nil {
		t.Fatalf("Build() large diff error = %v", err)
	}
	// Only the rendered byte count differs, so the growth is bounded by the
	// width of a decimal number rather than by the diff.
	if growth := len(large) - len(small); growth > 32 {
		t.Fatalf("prompt grew %d bytes with the diff, want a bounded reference", growth)
	}
}
