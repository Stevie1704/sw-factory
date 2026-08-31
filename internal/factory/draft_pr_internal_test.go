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

	tests := []struct {
		name        string
		issueNumber int
		want        string
		unwanted    string
	}{
		{name: "tracked issue", issueNumber: 42, want: "\n\nCloses #42\n"},
		{name: "unidentified issue", issueNumber: 0, unwanted: "Closes #"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			run := store.Run{ID: "run-close", IssueNumber: test.issueNumber, CheckpointSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"}
			body := generatedPullRequestBody(run, packet, nil, "none")
			if test.want != "" && !strings.Contains(body, test.want) {
				t.Fatalf("generated body = %q, want closing declaration %q", body, test.want)
			}
			if test.unwanted != "" && strings.Contains(body, test.unwanted) {
				t.Fatalf("generated body = %q, want no closing declaration for an unidentified issue", body)
			}
		})
	}
}
