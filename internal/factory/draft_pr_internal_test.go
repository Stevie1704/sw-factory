package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestGeneratedPullRequestBodyDeclaresTheClosingKeyword pins the reference that
// makes GitHub link the pull request to its issue and close that issue on
// merge. A bare `#42` mention does not, so the keyword is asserted separately
// from the issue identity line.
func TestGeneratedPullRequestBodyDeclaresTheClosingKeyword(t *testing.T) {
	t.Parallel()

	packet := SpecificationPacket{
		Version: 1,
		Issue:   github.Issue{Number: 42, Title: "Close the issue on merge", Body: "Link the pull request to its issue."},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
			TestPolicy:   config.TestPolicy{Mode: config.TestModeAdvisory},
		},
	}
	run := store.Run{ID: "run-close", IssueNumber: 42, CheckpointSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"}

	body := generatedPullRequestBody(run, packet, nil, "none")

	if !strings.Contains(body, "\n\nCloses #42\n") {
		t.Fatalf("generated body = %q, want a closing declaration for issue 42", body)
	}
	if !strings.Contains(body, generatedPullRequestStart) || !strings.Contains(body, generatedPullRequestEnd) {
		t.Fatalf("generated body = %q, want the factory-generated markers that carry the declaration", body)
	}
}
