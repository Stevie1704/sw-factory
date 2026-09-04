package prompt_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// reviewRequestWithDiff builds a specification-review prompt request with the
// bounded identity metadata for its mounted review artifact.
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
			DiffPath:      prompt.WorkerReviewDiffPath,
			DiffBytes:     int64(len(diff)),
			DiffSHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			RelevantLogs:  []prompt.ReviewLog{{Source: "gate:test", Detail: "passed at the exact checkpoint"}},
			PriorFindings: []store.ReviewFinding{{Location: "internal/store/store.go:1810", Claim: "the projection is still read"}},
		},
	}
}

// TestBuildReviewPromptPointsAtTheMountedDiff verifies the prompt keeps the
// bounded review observations and names the immutable artifact without carrying
// any diff-delivery branch in the packet context.
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
		fmt.Sprintf("%d bytes", len(diff)),
		prompt.WorkerReviewDiffPath,
		"diff_sha256",
		"sed -n '1,200p' /invocation/review.diff",
		"sed -n '201,400p' /invocation/review.diff",
	} {
		if !strings.Contains(value, present) {
			t.Fatalf("review prompt missing %q:\n%s", present, value)
		}
	}
	for _, absent := range []string{"removed line", "current_diff", "omitted_diff_bytes", "changed_paths_command", "diff_path_command", "changed nothing against its base"} {
		if strings.Contains(value, absent) {
			t.Fatalf("review prompt contains superseded delivery branch %q:\n%s", absent, value)
		}
	}
}

// TestBuildReviewPromptSizeDoesNotFollowTheDiff verifies a reviewed change can
// grow without the prompt growing with it.
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

// TestBuildReviewPromptDescribesAnEmptyArtifactWithoutASeparateBranch verifies
// a zero-byte review artifact still has one stable file-reading procedure.
func TestBuildReviewPromptDescribesAnEmptyArtifactWithoutASeparateBranch(t *testing.T) {
	request := reviewRequestWithDiff("")
	request.ReviewContext.DiffBytes = 0
	value, err := prompt.Build(request)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, present := range []string{"0 bytes", prompt.WorkerReviewDiffPath, "sed -n '1,200p' /invocation/review.diff"} {
		if !strings.Contains(value, present) {
			t.Fatalf("review prompt missing %q:\n%s", present, value)
		}
	}
	for _, absent := range []string{"changed nothing", "omitted", "present", "current_diff"} {
		if strings.Contains(strings.ToLower(value), absent) {
			t.Fatalf("empty review prompt contains delivery branch %q:\n%s", absent, value)
		}
	}
}
