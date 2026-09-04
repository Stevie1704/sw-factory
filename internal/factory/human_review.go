package factory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// humanReviewCategory classifies every human repair instruction. An authorized
// maintainer's requested change is authoritative product intent, so it uses the
// declared specification category rather than widening the closed vocabulary.
const humanReviewCategory = report.ReviewCategorySpecification

// humanReviewerRole names the reviewer that produced a human repair packet.
// It is deliberately not a workflow role: no invocation, prompt, or stage
// belongs to it, and it exists only to preserve finding provenance.
const humanReviewerRole = "human"

// pullRequestReviewReader resolves the read-only review seam without widening
// the pull-request mutation authority of the other GitHub clients.
func (s *Service) pullRequestReviewReader() github.PullRequestReviewReader {
	if s.deps.PullRequestReviews != nil {
		return s.deps.PullRequestReviews
	}
	reader, _ := s.deps.GitHub.(github.PullRequestReviewReader)
	return reader
}

// consumeHumanReview applies at most one newly submitted `CHANGES_REQUESTED`
// review from an authorized maintainer as a single implementation repair
// packet.
//
// Only a completed review decision is a progression event. An unsubmitted
// draft, an ordinary comment, a `COMMENTED` review, an approval, a dismissed
// decision, and a review from an unauthorized user are all read and ignored.
func (s *Service) consumeHumanReview(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) error {
	if !humanReviewEligible(run) {
		return nil
	}
	reader := s.pullRequestReviewReader()
	if reader == nil {
		return nil
	}
	active, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return fmt.Errorf("look up active invocations before human review: %w", err)
	}
	if !supported || len(active) > 0 {
		// A human review must never interrupt a harness session that can still
		// write a structured result for the current checkpoint.
		return nil
	}
	reviews, err := reader.PullRequestReviews(ctx, commandRepository(registration), run.PullRequestNumber)
	if err != nil {
		return fmt.Errorf("read reviews for pull request #%d: %w", run.PullRequestNumber, err)
	}
	applicable := applicableHumanReviews(reviews, registration.AuthorizedUsers, run.ProcessedReviewID)
	if len(applicable) == 0 {
		return nil
	}
	findings := make([]store.ReviewRepairFinding, 0, len(applicable))
	for _, review := range applicable {
		findings = append(findings, humanRepairFindings(run, review)...)
	}
	if len(findings) == 0 {
		return nil
	}
	return s.applyHumanRepair(ctx, registration, runStore, run, applicable, findings)
}

// applyHumanRepair returns the pull request to draft, resumes implementation
// from the current checkpoint, and persists the review watermark.
//
// The draft change comes first and is idempotent, while the watermark advances
// only with the durable transition. An interrupted pass therefore repeats the
// draft change instead of losing the review.
func (s *Service) applyHumanRepair(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, applicable []github.PullRequestReview, findings []store.ReviewRepairFinding) error {
	if err := s.returnPullRequestToDraft(ctx, registration, run); err != nil {
		return err
	}
	watermark := applicable[len(applicable)-1].ID
	next := humanRepairProjection(run, watermark, fmt.Sprintf("authorized human review %s requested changes; resuming implementation from checkpoint %s", watermark, run.CheckpointSHA), findings)
	next.ProcessedReviewID = watermark
	next.Revision = run.Revision + 1
	next.ProcessedReviewRevision = next.Revision
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
		return fmt.Errorf("persist human review repair state: %w", err)
	}
	return nil
}

// humanRepairProjection applies the maintainer-repair transition shared by
// every human disposition surface: implementation resumes from the current
// checkpoint against the supplied findings. The caller owns the watermark that
// admits its own trigger exactly once, because a submitted review and a
// supervision comment are watermarked separately.
func humanRepairProjection(run store.Run, watermark, reason string, findings []store.ReviewRepairFinding) store.Run {
	next := run
	next.Stage = store.StageImplementation
	next.Status = store.StatusActive
	next.LifecycleReason = reason
	next.ReviewRepairPacket = &store.ReviewRepairPacket{
		Version:       1,
		Source:        store.ReviewRepairSourceHuman,
		ReviewID:      watermark,
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Budget:        run.ReviewRepairBudget,
		Findings:      findings,
	}
	// The reviewed checkpoint is superseded, so every result derived from it
	// stops being evidence for readiness.
	next.SpecificationReview = nil
	next.StandardsReview = nil
	next.ReviewRepairPendingAttempt = 0
	next.ReadyNotificationSent = false
	// An outstanding reviewer question was asked about the superseded
	// checkpoint, and the maintainer has now given direction for it. Clearing
	// it matches how a factory repair round supersedes the same question.
	next.PendingQuestions = nil
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	return next
}

// humanReviewEligible reports whether the run has a tracked pull request whose
// completed reviews can still change the outcome of the run.
func humanReviewEligible(run store.Run) bool {
	if run.PullRequestNumber == 0 || store.IsTerminalStatus(run.Status) {
		return false
	}
	if run.Status != store.StatusActive && run.Status != store.StatusWaitingForHuman {
		return false
	}
	switch run.Stage {
	case store.StageDraftPR, store.StageReview, store.StageReady:
		return true
	default:
		return false
	}
}

// applicableHumanReviews returns every submitted `CHANGES_REQUESTED` review
// from an authorized maintainer that the persisted watermark has not already
// applied, oldest identity first.
//
// Two maintainers can request changes between two polls. Returning all of them
// lets one repair packet carry every outstanding instruction, so advancing the
// watermark to the newest identity cannot discard an older unapplied review.
func applicableHumanReviews(reviews []github.PullRequestReview, authorized []string, watermark string) []github.PullRequestReview {
	applicable := make([]github.PullRequestReview, 0, len(reviews))
	for _, review := range reviews {
		if review.State != github.PullRequestReviewChangesRequested {
			continue
		}
		if review.SubmittedAt.IsZero() || strings.TrimSpace(review.ID) == "" {
			continue
		}
		if !authorizedCommentAuthor(authorized, review.Author) {
			continue
		}
		if githubIDAlreadyProcessed(watermark, review.ID) {
			continue
		}
		applicable = append(applicable, review)
	}
	sort.SliceStable(applicable, func(left, right int) bool {
		return compareGitHubIDs(applicable[left].ID, applicable[right].ID) < 0
	})
	return applicable
}

// humanRepairFindings converts one completed review into blocking findings.
// The review body is one finding and every inline comment is another, so the
// implementation role receives the complete human instruction.
func humanRepairFindings(run store.Run, review github.PullRequestReview) []store.ReviewRepairFinding {
	evidence := fmt.Sprintf("GitHub review %s submitted by %s", review.ID, review.Author)
	findings := make([]store.ReviewRepairFinding, 0, len(review.Comments)+1)
	if body := strings.TrimSpace(review.Body); body != "" {
		findings = append(findings, humanRepairFinding(fmt.Sprintf("pull request #%d", run.PullRequestNumber), body, evidence))
	}
	for _, comment := range review.Comments {
		claim := strings.TrimSpace(comment.Body)
		if claim == "" {
			continue
		}
		findings = append(findings, humanRepairFinding(humanReviewLocation(comment), claim, evidence))
	}
	return findings
}

// humanRepairFinding renders one human instruction in the existing review
// finding contract. Implementation always owns the repair: a human comment on
// a protected test still reaches the test role through the objection protocol
// only when a factory reviewer assigns that ownership.
func humanRepairFinding(location, claim, evidence string) store.ReviewRepairFinding {
	return store.ReviewRepairFinding{
		ReviewerRole: humanReviewerRole,
		Finding: store.ReviewFinding{
			Location:            location,
			Claim:               claim,
			Evidence:            evidence,
			Severity:            string(report.ReviewSeverityBlocker),
			Category:            string(humanReviewCategory),
			SuggestedResolution: claim,
			SuggestedOwner:      workflow.RoleImplementation,
		},
	}
}

// humanReviewLocation renders the repository-relative anchor of one inline
// review comment in the same shape reviewers use for their own findings.
func humanReviewLocation(comment github.PullRequestReviewComment) string {
	path := strings.TrimSpace(comment.Path)
	if path == "" {
		return "pull request diff"
	}
	if comment.Line > 0 {
		return fmt.Sprintf("%s:%d", path, comment.Line)
	}
	return path
}

// returnPullRequestToDraft removes readiness before implementation resumes, so
// a pull request awaiting human merge cannot be merged mid-repair. It is a
// no-op for a pull request that is already a draft.
func (s *Service) returnPullRequestToDraft(ctx context.Context, registration config.RepositoryRegistration, run store.Run) error {
	pullRequest, found, err := s.trackedPullRequest(ctx, registration, run)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("tracked pull request #%d was not found before human repair", run.PullRequestNumber)
	}
	if pullRequest.Draft {
		return nil
	}
	reverted, err := s.setPullRequestDraft(ctx, commandRepository(registration), pullRequest, true)
	if err != nil {
		return err
	}
	if !reverted.Draft {
		return fmt.Errorf("pull request #%d remained ready before human repair", pullRequest.Number)
	}
	return nil
}
