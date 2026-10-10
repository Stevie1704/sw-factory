package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestRunRetainsThePullRequestSummary verifies the saved PR-writer result
// survives a store round trip, so a readiness retry can reuse it.
func TestRunRetainsThePullRequestSummary(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	summary := &store.PullRequestSummary{
		CheckpointSHA: "0123456789abcdef0123456789abcdef01234567",
		InvocationID:  "inv-pr",
		Status:        store.PullRequestSummaryWritten,
		Body:          "## Summary\n\nAdds a role.",
	}
	if err := opened.SaveRun(t.Context(), store.Run{
		ID: "run-1", RepositoryPath: "/repo", IssueNumber: 1,
		Stage: store.StageReview, Status: store.StatusActive,
		Branch: "factory/run-1", Worktree: "/worktrees/run-1",
		CheckpointSHA:      "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		PullRequestSummary: summary,
		CreatedAt:          base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	run, err := opened.CurrentRun(t.Context())
	if err != nil || run == nil {
		t.Fatalf("CurrentRun() = %#v, %v", run, err)
	}
	if run.PullRequestSummary == nil || *run.PullRequestSummary != *summary {
		t.Fatalf("PullRequestSummary = %#v, want %#v", run.PullRequestSummary, summary)
	}
}

// TestSaveRunRejectsAnInconsistentPullRequestSummary verifies a written
// summary needs a body and a failed summary keeps none.
func TestSaveRunRejectsAnInconsistentPullRequestSummary(t *testing.T) {
	t.Parallel()

	sha := "0123456789abcdef0123456789abcdef01234567"
	cases := map[string]store.PullRequestSummary{
		"written without body": {CheckpointSHA: sha, Status: store.PullRequestSummaryWritten},
		"failed with body":     {CheckpointSHA: sha, Status: store.PullRequestSummaryFailed, Body: "text"},
		"unknown status":       {CheckpointSHA: sha, Status: "pending"},
		"invalid checkpoint":   {CheckpointSHA: "main", Status: store.PullRequestSummaryFailed},
		"multi-line reason":    {CheckpointSHA: sha, Status: store.PullRequestSummaryFailed, Reason: "a\nb"},
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			opened, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state", "factory.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = opened.Close() }()
			base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
			err = opened.SaveRun(t.Context(), store.Run{
				ID: "run-1", RepositoryPath: "/repo", IssueNumber: 1,
				Stage: store.StageReview, Status: store.StatusActive,
				Branch: "factory/run-1", Worktree: "/worktrees/run-1",
				PullRequestSummary: &summary, CreatedAt: base, UpdatedAt: base,
			})
			if err == nil {
				t.Fatal("SaveRun() error = nil, want an inconsistent summary refused")
			}
		})
	}
}
