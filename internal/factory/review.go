package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// SpecificationReviewStatusContext is the stable exact-SHA status context for
// the independent specification reviewer.
const SpecificationReviewStatusContext = "factory/specification-review"

// collectReviewContext obtains the immutable diff through the worker boundary
// and carries only bounded handoffs and content-free checkpoint evidence.
func collectReviewContext(ctx context.Context, runtime worker.WorkerRuntime, runID, checkpoint string, run store.Run) (*ReviewContext, error) {
	if runtime == nil {
		return nil, errors.New("worker runtime is required for specification review")
	}
	if !github.ValidCommitSHA(checkpoint) {
		return nil, errors.New("specification review requires a valid checkpoint SHA")
	}
	commandRuntime, ok := runtime.(interface {
		RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error)
	})
	if !ok {
		return nil, errors.New("worker runtime does not support specification-review context collection")
	}
	result, err := commandRuntime.RunCommand(ctx, worker.CommandRequest{
		RunID:             runID,
		Command:           fmt.Sprintf("git diff --no-ext-diff %s^ %s --", checkpoint, checkpoint),
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "spec_review",
	})
	if err != nil {
		return nil, fmt.Errorf("collect exact checkpoint diff: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("collect exact checkpoint diff exited with code %d", result.ExitCode)
	}
	return &ReviewContext{
		CheckpointSHA:         checkpoint,
		CurrentDiff:           result.Stdout,
		RelevantLogs:          []string{"review context collected for exact checkpoint " + checkpoint},
		ImplementationHandoff: run.ImplementationHandoff,
		TestHandoff:           run.TestHandoff,
		TestExemption:         run.TestExemption,
	}, nil
}

// publishSpecificationReviewStatus publishes a status attached to the exact
// checkpoint and fails closed when the configured publisher is unavailable.
func (s *Service) publishSpecificationReviewStatus(ctx context.Context, registration config.RepositoryRegistration, sha string, state github.CommitStatusState, description string) error {
	if s.deps.CommitStatuses == nil {
		return errors.New("commit status publisher is required for specification review")
	}
	return s.deps.CommitStatuses.CreateCommitStatus(ctx, github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}, github.CommitStatus{
		SHA: sha, State: state, Context: SpecificationReviewStatusContext, Description: description,
	})
}

// acceptSpecificationReviewReport stores the exact-checkpoint review and
// routes concrete violations to human review while keeping advisories visible.
func (s *Service) acceptSpecificationReviewReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if value.ReviewHandoff == nil {
		return AgentResult{}, errors.New("completed specification review requires a review handoff")
	}
	projection := &store.SpecificationReview{CheckpointSHA: value.ReviewHandoff.ReviewedSHA}
	blocking := false
	for _, finding := range value.ReviewHandoff.Findings {
		projection.Findings = append(projection.Findings, store.ReviewFinding{
			Location: finding.Location, Claim: finding.Claim, Evidence: finding.Evidence,
			Severity: string(finding.Severity), Category: string(finding.Category),
			SuggestedResolution: finding.SuggestedResolution, SuggestedOwner: finding.SuggestedOwner,
		})
		if report.ReviewFindingBlocks(finding) {
			blocking = true
		}
	}
	previous := *run
	run.SpecificationReview = projection
	run.Stage = store.StageReview
	run.PendingQuestions = nil
	run.ClarificationCommentID = ""
	run.ClarificationNotificationSent = false
	if blocking {
		run.Status = store.StatusWaitingForHuman
		run.LifecycleReason = "specification review found a concrete violation"
	} else {
		run.Status = store.StatusActive
		run.Stage = store.StageReady
		run.LifecycleReason = "specification review accepted; advisories remain visible"
	}
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist specification review state: %w", err)
	}
	status := github.CommitStatusSuccess
	description := "specification review accepted"
	if blocking {
		status = github.CommitStatusFailure
		description = "specification review requires human disposition"
	}
	if err := s.publishSpecificationReviewStatus(ctx, registration, value.ReviewHandoff.ReviewedSHA, status, description); err != nil {
		return AgentResult{}, err
	}
	if run.PullRequestNumber > 0 {
		packet, err := decodeSpecificationPacket(run.SpecificationPacket)
		if err != nil {
			return AgentResult{}, err
		}
		if _, err := s.regenerateDraftPullRequest(ctx, registration, *run, packet, ""); err != nil {
			return AgentResult{}, fmt.Errorf("project specification review on pull request: %w", err)
		}
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// reviewFindingLine renders a complete finding without exposing a transcript.
func reviewFindingLine(finding store.ReviewFinding) string {
	return fmt.Sprintf("- `%s` `%s` at `%s`: %s — evidence: %s; suggested resolution: %s; suggested owner: %s", finding.Severity, finding.Category, finding.Location, finding.Claim, finding.Evidence, finding.SuggestedResolution, finding.SuggestedOwner)
}

// renderSpecificationReview projects review findings into the generated PR section.
func renderSpecificationReview(review *store.SpecificationReview) string {
	if review == nil {
		return ""
	}
	lines := make([]string, 0, len(review.Findings))
	for _, finding := range review.Findings {
		lines = append(lines, reviewFindingLine(finding))
	}
	if len(lines) == 0 {
		lines = append(lines, "- no findings")
	}
	return fmt.Sprintf("\n### Specification review\n\n- reviewed checkpoint: `%s`\n%s\n", review.CheckpointSHA, strings.Join(lines, "\n"))
}

// renderSpecificationReviewStatus projects findings into the editable status
// comment while keeping every untrusted field on one safe Markdown line.
func renderSpecificationReviewStatus(review *store.SpecificationReview) string {
	if review == nil {
		return ""
	}
	lines := []string{"\n### Specification review", "", "- reviewed checkpoint: `" + safeStatusCommentValue(review.CheckpointSHA) + "`"}
	if len(review.Findings) == 0 {
		lines = append(lines, "- no findings")
	} else {
		for _, finding := range review.Findings {
			lines = append(lines, fmt.Sprintf("- `%s` `%s` at `%s`: %s — evidence: %s; suggested resolution: %s; suggested owner: %s",
				safeStatusCommentValue(finding.Severity), safeStatusCommentValue(finding.Category), safeStatusCommentValue(finding.Location),
				safeStatusCommentValue(finding.Claim), safeStatusCommentValue(finding.Evidence), safeStatusCommentValue(finding.SuggestedResolution), safeStatusCommentValue(finding.SuggestedOwner)))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
