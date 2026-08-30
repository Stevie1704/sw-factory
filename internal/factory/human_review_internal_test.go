package factory

import (
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// submittedAt is one non-zero submission time shared by the review fixtures.
var submittedAt = time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)

// TestApplicableHumanReviewsAcceptOnlyAuthorizedCompletedDecisions proves that
// only a submitted `CHANGES_REQUESTED` review from an authorized maintainer
// starts work, that concurrent decisions are all carried, and that the
// persisted watermark makes repeated polling replay-safe.
func TestApplicableHumanReviewsAcceptOnlyAuthorizedCompletedDecisions(t *testing.T) {
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
		wantIDs   []string
	}{
		{
			name:    "authorized changes requested",
			reviews: []github.PullRequestReview{changesRequested},
			wantIDs: []string{"40"},
		},
		{
			name: "concurrent authorized decisions are all carried, oldest first",
			reviews: []github.PullRequestReview{
				{ID: "41", Author: "bob", State: github.PullRequestReviewChangesRequested, Body: "and rename the flag", SubmittedAt: submittedAt},
				changesRequested,
			},
			wantIDs: []string{"40", "41"},
		},
		{
			name: "watermark keeps only the unapplied decision",
			reviews: []github.PullRequestReview{
				changesRequested,
				{ID: "41", Author: "bob", State: github.PullRequestReviewChangesRequested, Body: "and rename the flag", SubmittedAt: submittedAt},
			},
			watermark: "40",
			wantIDs:   []string{"41"},
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
			applicable := applicableHumanReviews(test.reviews, authorized, test.watermark)
			if len(applicable) != len(test.wantIDs) {
				t.Fatalf("applicable reviews = %d, want %d", len(applicable), len(test.wantIDs))
			}
			for index, review := range applicable {
				if review.ID != test.wantIDs[index] {
					t.Fatalf("applicable review %d = %q, want %q", index, review.ID, test.wantIDs[index])
				}
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

// TestStatusCommentPublishesTheUnbudgetedHumanRepair verifies an operator can
// see that a human review, not a factory reviewer, requested the current
// repair, and that it does not consume the bounded repair budget.
func TestStatusCommentPublishesTheUnbudgetedHumanRepair(t *testing.T) {
	t.Parallel()

	run := store.Run{
		ID:                 "run-human",
		ReviewRepairBudget: 2,
		ReviewRepairPacket: &store.ReviewRepairPacket{
			Version: 1, Source: store.ReviewRepairSourceHuman, ReviewID: "4001",
			RunID: "run-human", Budget: 2,
		},
	}
	body := reviewRepairStatusComment(run)
	if !strings.Contains(body, "- human review: 4001 (does not consume the budget)") {
		t.Fatalf("status comment = %q, want the published human repair trigger", body)
	}
	if !strings.Contains(body, "- attempts: 0/2") {
		t.Fatalf("status comment = %q, want an unchanged factory repair budget", body)
	}
}

// TestConsumesBoundedRepairRoundExcludesHumanRequestedRepair verifies that only
// a factory packet consumes a reserved repair round. Every implementation
// launch that carries a packet reaches this rule, including the test-stage
// handoff, so a human review must not reset an exhausted repair budget.
func TestConsumesBoundedRepairRoundExcludesHumanRequestedRepair(t *testing.T) {
	t.Parallel()

	factoryPacket := &store.ReviewRepairPacket{Version: 1, Attempt: 2, Budget: 2}
	humanPacket := &store.ReviewRepairPacket{Version: 1, Source: store.ReviewRepairSourceHuman, ReviewID: "4001", Budget: 2}
	tests := []struct {
		name         string
		reviewRepair bool
		packet       *store.ReviewRepairPacket
		want         bool
	}{
		{name: "factory packet on a repair launch", reviewRepair: true, packet: factoryPacket, want: true},
		{name: "legacy packet without a source", reviewRepair: true, packet: &store.ReviewRepairPacket{Version: 1, Attempt: 1, Budget: 2}, want: true},
		{name: "human packet on a repair launch", reviewRepair: true, packet: humanPacket},
		{name: "factory packet on an ordinary launch", packet: factoryPacket},
		{name: "no packet", reviewRepair: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := consumesBoundedRepairRound(test.reviewRepair, test.packet); got != test.want {
				t.Fatalf("consumesBoundedRepairRound = %t, want %t", got, test.want)
			}
		})
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
