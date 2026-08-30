package factory

import (
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// submittedAt is one non-zero submission time shared by the review fixtures.
var submittedAt = time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)

// TestNewestApplicableHumanReviewAcceptsOnlyAnAuthorizedCompletedDecision
// proves that only a submitted `CHANGES_REQUESTED` review from an authorized
// maintainer starts work, and that the persisted watermark makes repeated
// polling replay-safe.
func TestNewestApplicableHumanReviewAcceptsOnlyAnAuthorizedCompletedDecision(t *testing.T) {
	t.Parallel()

	authorized := []string{"alice", "bob"}
	changesRequested := github.PullRequestReview{
		ID: "40", Author: "alice", State: github.PullRequestReviewChangesRequested,
		Body: "restore the deleted validation", SubmittedAt: submittedAt,
	}
	tests := []struct {
		name      string
		reviews   []github.PullRequestReview
		watermark string
		wantID    string
	}{
		{
			name:    "authorized changes requested",
			reviews: []github.PullRequestReview{changesRequested},
			wantID:  "40",
		},
		{
			name: "newest authorized decision wins",
			reviews: []github.PullRequestReview{
				changesRequested,
				{ID: "41", Author: "bob", State: github.PullRequestReviewChangesRequested, Body: "and rename the flag", SubmittedAt: submittedAt},
			},
			wantID: "41",
		},
		{
			name:      "already applied review is skipped",
			reviews:   []github.PullRequestReview{changesRequested},
			watermark: "40",
		},
		{
			name:      "older review below the watermark is skipped",
			reviews:   []github.PullRequestReview{changesRequested},
			watermark: "55",
		},
		{
			name:    "unauthorized author is ignored",
			reviews: []github.PullRequestReview{{ID: "42", Author: "mallory", State: github.PullRequestReviewChangesRequested, Body: "rewrite it", SubmittedAt: submittedAt}},
		},
		{
			name:    "commented review is ignored",
			reviews: []github.PullRequestReview{{ID: "43", Author: "alice", State: github.PullRequestReviewCommented, Body: "looks interesting", SubmittedAt: submittedAt}},
		},
		{
			name:    "approval is ignored",
			reviews: []github.PullRequestReview{{ID: "44", Author: "alice", State: github.PullRequestReviewApproved, Body: "ship it", SubmittedAt: submittedAt}},
		},
		{
			name:    "dismissed decision is ignored",
			reviews: []github.PullRequestReview{{ID: "45", Author: "alice", State: github.PullRequestReviewDismissed, Body: "withdrawn", SubmittedAt: submittedAt}},
		},
		{
			name:    "unsubmitted draft is ignored",
			reviews: []github.PullRequestReview{{ID: "46", Author: "alice", State: github.PullRequestReviewChangesRequested, Body: "still drafting"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selected, found := newestApplicableHumanReview(test.reviews, authorized, test.watermark)
			if found != (test.wantID != "") {
				t.Fatalf("found = %t, want %t", found, test.wantID != "")
			}
			if found && selected.ID != test.wantID {
				t.Fatalf("selected review = %q, want %q", selected.ID, test.wantID)
			}
		})
	}
}

// TestHumanRepairFindingsCarryTheBodyAndEveryInlineComment verifies that one
// completed review becomes a single implementation-owned repair packet holding
// the review body and its file-anchored findings.
func TestHumanRepairFindingsCarryTheBodyAndEveryInlineComment(t *testing.T) {
	t.Parallel()

	review := github.PullRequestReview{
		ID: "40", Author: "alice", State: github.PullRequestReviewChangesRequested,
		Body: "restore the deleted validation", SubmittedAt: submittedAt,
		Comments: []github.PullRequestReviewComment{
			{Path: "internal/factory/review.go", Line: 42, Body: "this branch cannot be reached"},
			{Path: "internal/factory/gate.go", Body: "no line anchor here"},
			{Path: "internal/factory/lock.go", Line: 7, Body: "   "},
		},
	}
	findings := humanRepairFindings(store.Run{PullRequestNumber: 17}, review)
	if len(findings) != 3 {
		t.Fatalf("findings = %d, want the body and the two non-empty inline comments", len(findings))
	}
	wantLocations := []string{"pull request #17", "internal/factory/review.go:42", "internal/factory/gate.go"}
	for index, finding := range findings {
		if finding.Finding.Location != wantLocations[index] {
			t.Fatalf("finding %d location = %q, want %q", index, finding.Finding.Location, wantLocations[index])
		}
		if finding.ReviewerRole != humanReviewerRole {
			t.Fatalf("finding %d reviewer = %q, want %q", index, finding.ReviewerRole, humanReviewerRole)
		}
		if finding.Finding.SuggestedOwner != workflow.RoleImplementation {
			t.Fatalf("finding %d owner = %q, want implementation", index, finding.Finding.SuggestedOwner)
		}
		if finding.Finding.Severity != string(report.ReviewSeverityBlocker) {
			t.Fatalf("finding %d severity = %q, want blocker", index, finding.Finding.Severity)
		}
		if !reviewFindingBlocks(finding.Finding) {
			t.Fatalf("finding %d does not block readiness", index)
		}
	}
}

// TestHumanReviewEligibleCoversOnlyRunsWithAPendingPullRequestDecision
// verifies that the coordinator reads reviews exactly while a tracked pull
// request can still change the outcome of the run.
func TestHumanReviewEligibleCoversOnlyRunsWithAPendingPullRequestDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  store.Run
		want bool
	}{
		{name: "review stage", run: store.Run{PullRequestNumber: 17, Stage: store.StageReview, Status: store.StatusActive}, want: true},
		{name: "ready stage awaiting merge", run: store.Run{PullRequestNumber: 17, Stage: store.StageReady, Status: store.StatusWaitingForHuman}, want: true},
		{name: "draft pull request", run: store.Run{PullRequestNumber: 17, Stage: store.StageDraftPR, Status: store.StatusActive}, want: true},
		{name: "no pull request yet", run: store.Run{Stage: store.StageImplementation, Status: store.StatusActive}},
		{name: "implementation already resumed", run: store.Run{PullRequestNumber: 17, Stage: store.StageImplementation, Status: store.StatusActive}},
		{name: "merged run", run: store.Run{PullRequestNumber: 17, Stage: store.StageReady, Status: store.StatusComplete}},
		{name: "cancelled run", run: store.Run{PullRequestNumber: 17, Stage: store.StageReady, Status: store.StatusCancelled}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := humanReviewEligible(test.run); got != test.want {
				t.Fatalf("humanReviewEligible = %t, want %t", got, test.want)
			}
		})
	}
}
