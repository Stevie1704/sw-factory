package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// SpecificationReviewStatusContext is the stable exact-SHA status context
	// used by every specification-review invocation.
	SpecificationReviewStatusContext = "factory/review/specification"
	// maxReviewDiffBytes bounds the immutable diff copied into a review packet.
	maxReviewDiffBytes = 512 << 10
)

// roleHandoffFromReport converts the report protocol's completed handoff into
// the durable reviewer-facing role projection.
func roleHandoffFromReport(value report.Handoff) *store.RoleHandoff {
	result := &store.RoleHandoff{
		ChangeSummary:          value.ChangeSummary,
		ProductionFilesChanged: append([]string(nil), value.ProductionFilesChanged...),
		FocusedCommands:        append([]string(nil), value.FocusedCommands...),
		KnownLimitations:       append([]string(nil), value.KnownLimitations...),
		TestObjections:         make([]store.TestObjection, 0, len(value.TestObjections)),
		AcceptanceMapping:      make([]store.HandoffAcceptance, 0, len(value.AcceptanceMapping)),
	}
	for _, mapping := range value.AcceptanceMapping {
		result.AcceptanceMapping = append(result.AcceptanceMapping, store.HandoffAcceptance{
			Criterion: mapping.Criterion,
			Evidence:  mapping.Evidence,
		})
	}
	for _, objection := range value.TestObjections {
		result.TestObjections = append(result.TestObjections, store.TestObjection{
			Test:     objection.Test,
			Claim:    objection.Claim,
			Evidence: objection.Evidence,
		})
	}
	return result
}

// reviewContextForRun assembles the reviewer input from durable projections.
// Gate results are reduced to outcome metadata; command transcripts never enter
// the packet.
func reviewContextForRun(ctx context.Context, run store.Run, runStore RunStore) (*prompt.ReviewContext, error) {
	contextValue := &prompt.ReviewContext{
		CheckpointSHA:         run.CheckpointSHA,
		TestHandoff:           run.TestHandoff,
		ImplementationHandoff: roleHandoffForRun(run),
		ProtectedTestPaths:    append([]store.ProtectedTestPath(nil), run.ProtectedTestPaths...),
		TestExemption:         run.TestExemption,
	}
	if run.SpecificationReview != nil && run.SpecificationReview.CheckpointSHA == run.CheckpointSHA {
		contextValue.PriorFindings = append([]store.ReviewFinding(nil), run.SpecificationReview.Findings...)
	}
	if resultStore, ok := runStore.(GateResultStore); ok {
		results, err := resultStore.GateResults(ctx, run.ID, store.GatePhaseCheckpoint, run.CheckpointSHA)
		if err != nil {
			return nil, fmt.Errorf("read exact-checkpoint gate results for review: %w", err)
		}
		for _, result := range results {
			detail := fmt.Sprintf("outcome=%s status=%s blocking=%t", result.Outcome, result.Status, result.Blocking)
			if result.SkipReason != "" {
				detail += " skip_reason=" + result.SkipReason
			}
			contextValue.RelevantLogs = append(contextValue.RelevantLogs, prompt.ReviewLog{
				Source: result.GateName,
				Detail: reviewSingleLine(detail),
			})
		}
	}
	return contextValue, nil
}

// ensureReviewStart verifies the reviewer receives a clean, immutable draft
// pull-request checkpoint before any worker or harness side effect occurs.
func (s *Service) ensureReviewStart(ctx context.Context, run store.Run) error {
	if run.Stage != store.StageDraftPR {
		return fmt.Errorf("specification review requires draft_pr stage, not %q", run.Stage)
	}
	if run.PullRequestNumber <= 0 {
		return errors.New("specification review requires a tracked pull request")
	}
	if !github.ValidCommitSHA(run.CheckpointSHA) {
		return errors.New("specification review requires a valid checkpoint SHA")
	}
	base := run.BaseCheckpointSHA
	if base == "" {
		base = run.CheckpointSHA
	}
	if !github.ValidCommitSHA(base) {
		return errors.New("specification review requires a valid base checkpoint SHA")
	}
	if run.SpecificationReview != nil && run.SpecificationReview.CheckpointSHA == run.CheckpointSHA {
		return errors.New("specification review already has a result for this checkpoint")
	}
	inspector := s.worktreeInspector()
	if inspector == nil {
		return errors.New("worktree runtime is required for immutable specification review")
	}
	state, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		return fmt.Errorf("inspect worktree before specification review: %w", err)
	}
	if state.HeadSHA != run.CheckpointSHA {
		return fmt.Errorf("review worktree HEAD %q does not match checkpoint %q", state.HeadSHA, run.CheckpointSHA)
	}
	if len(state.ChangedPaths) != 0 {
		return errors.New("specification review requires a clean checkpoint worktree")
	}
	return nil
}

// captureReviewDiff obtains the exact base-to-checkpoint diff inside the
// pinned worker after its review role mount has been established.
func (s *Service) captureReviewDiff(ctx context.Context, run store.Run, role string) (string, error) {
	if s.deps.Worker == nil {
		return "", errors.New("worker runtime is required to capture the review diff")
	}
	base := run.BaseCheckpointSHA
	if base == "" {
		base = run.CheckpointSHA
	}
	if !github.ValidCommitSHA(base) || !github.ValidCommitSHA(run.CheckpointSHA) {
		return "", errors.New("review diff requires valid immutable base and checkpoint SHAs")
	}
	result, err := s.deps.Worker.RunCommand(ctx, worker.CommandRequest{
		RunID:             run.ID,
		Command:           fmt.Sprintf("git diff --no-ext-diff --binary --unified=80 %s %s -- .", base, run.CheckpointSHA),
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              role,
	})
	if err != nil {
		return "", fmt.Errorf("capture exact review diff: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("capture exact review diff exited with code %d", result.ExitCode)
	}
	if len([]byte(result.Stdout)) > maxReviewDiffBytes {
		return "", fmt.Errorf("review diff exceeds %d-byte packet limit", maxReviewDiffBytes)
	}
	return result.Stdout, nil
}

// publishSpecificationReviewStatus publishes one exact-SHA reviewer status.
// The stable context is deliberately independent of invocation identity so a
// later review replaces the status for the same checkpoint rather than adding
// an unrelated status stream.
func (s *Service) publishSpecificationReviewStatus(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, state github.CommitStatusState, description string) error {
	if s.deps.CommitStatuses == nil {
		return errors.New("commit-status publisher is required for specification review")
	}
	if !github.ValidCommitSHA(run.CheckpointSHA) {
		return errors.New("specification review requires a valid checkpoint SHA")
	}
	description = reviewStatusDescription(description)
	statuses := github.CommitStatusPublisher(s.deps.CommitStatuses)
	statuses = commitStatusPublisher{service: s, runStore: runStore, runID: run.ID, delegate: statuses}
	return statuses.CreateCommitStatus(ctx, github.Repository{
		Owner: registration.GitHub.Owner,
		Name:  registration.GitHub.Repository,
	}, github.CommitStatus{
		SHA:         run.CheckpointSHA,
		State:       state,
		Context:     SpecificationReviewStatusContext,
		Description: description,
		TargetURL:   run.PullRequestURL,
	})
}

// reviewStatusDescription keeps status text bounded and free of formatting
// control characters while preserving the useful result counts.
func reviewStatusDescription(value string) string {
	value = reviewSingleLine(value)
	runes := []rune(value)
	if len(runes) <= 140 {
		return value
	}
	return string(runes[:137]) + "..."
}

// reviewSingleLine removes control characters from bounded reviewer metadata.
func reviewSingleLine(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", " ").Replace(value))
}

// reviewFindingBlocks applies the report contract's category policy to a
// durable finding projection.
func reviewFindingBlocks(finding store.ReviewFinding) bool {
	return report.ReviewFindingBlocks(report.ReviewFinding{
		Severity: report.ReviewSeverity(finding.Severity),
		Category: report.ReviewCategory(finding.Category),
	})
}

// reviewFindingCounts separates concrete gating violations from visible
// advisories for every supervision surface.
func reviewFindingCounts(findings []store.ReviewFinding) (blocking, advisory int) {
	for _, finding := range findings {
		if reviewFindingBlocks(finding) {
			blocking++
		} else {
			advisory++
		}
	}
	return blocking, advisory
}

// reviewHasBlockingFinding reports whether the durable finding projection
// contains a concrete violation that gates readiness.
func reviewHasBlockingFinding(findings []store.ReviewFinding) bool {
	for _, finding := range findings {
		if reviewFindingBlocks(finding) {
			return true
		}
	}
	return false
}

// specificationReviewFromReport converts a validated completed review into a
// durable exact-checkpoint projection.
func specificationReviewFromReport(value report.ReviewHandoff) *store.SpecificationReview {
	result := &store.SpecificationReview{
		CheckpointSHA: value.ReviewedSHA,
		Findings:      make([]store.ReviewFinding, 0, len(value.Findings)),
	}
	for _, finding := range value.Findings {
		result.Findings = append(result.Findings, store.ReviewFinding{
			Location:            finding.Location,
			Claim:               finding.Claim,
			Evidence:            finding.Evidence,
			Severity:            string(finding.Severity),
			Category:            string(finding.Category),
			SuggestedResolution: finding.SuggestedResolution,
			SuggestedOwner:      finding.SuggestedOwner,
		})
	}
	return result
}

// acceptSpecificationReviewReport applies the review gate, publishes the
// exact-checkpoint status, and projects every finding to supervision surfaces.
func (s *Service) acceptSpecificationReviewReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if run.CheckpointSHA == "" || !github.ValidCommitSHA(run.CheckpointSHA) {
		return AgentResult{}, errors.New("cannot accept review without a valid immutable checkpoint")
	}
	roleDefinition, err := roleDefinitionForInvocation(*invocation)
	if err != nil {
		return AgentResult{}, err
	}
	previous := *run
	statusState := github.CommitStatusError
	statusDescription := "specification review requires human disposition"
	switch value.Outcome {
	case report.OutcomeCompleted:
		review := specificationReviewFromReport(*value.ReviewHandoff)
		run.SpecificationReview = review
		if reviewHasBlockingFinding(review.Findings) {
			run.Stage = invocation.Stage
			run.Status = store.StatusWaitingForHuman
			run.LifecycleReason = "specification review found blocking violations"
			statusState = github.CommitStatusFailure
			statusDescription = "specification review found blocking findings"
		} else {
			transition, transitionErr := workflow.DefaultRegistry().ResolveReportTransition(invocation.Stage, value.Outcome)
			if transitionErr != nil {
				return AgentResult{}, transitionErr
			}
			run.Stage = transition.Stage
			run.Status = transition.Status
			run.LifecycleReason = "specification review accepted; ready for merge"
			statusState = github.CommitStatusSuccess
			statusDescription = fmt.Sprintf("specification review passed; %d advisory findings", len(review.Findings))
		}
		run.PendingQuestions = nil
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	case report.OutcomeNeedsClarification:
		run.SpecificationReview = nil
		transition, transitionErr := workflow.DefaultRegistry().ResolveReportTransition(invocation.Stage, value.Outcome)
		if transitionErr != nil {
			return AgentResult{}, transitionErr
		}
		run.Stage = transition.Stage
		run.Status = transition.Status
		run.LifecycleReason = "specification reviewer requested clarification"
		run.PendingQuestions = pendingQuestionsFromReport(value.Questions)
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	case report.OutcomeCannotProceed:
		run.SpecificationReview = nil
		transition, transitionErr := workflow.DefaultRegistry().ResolveReportTransition(invocation.Stage, value.Outcome)
		if transitionErr != nil {
			return AgentResult{}, transitionErr
		}
		run.Stage = transition.Stage
		run.Status = transition.Status
		run.LifecycleReason = "specification reviewer cannot proceed"
		run.PendingQuestions = nil
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	default:
		return AgentResult{}, fmt.Errorf("unsupported specification review outcome %q", value.Outcome)
	}
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.publishSpecificationReviewStatus(ctx, registration, runStore, *run, statusState, statusDescription); err != nil {
		return AgentResult{}, fmt.Errorf("publish specification review status: %w", err)
	}
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist specification review state: %w", err)
	}
	if value.Outcome == report.OutcomeCompleted && roleDefinition.Kind == workflow.RoleKindReview {
		if err := s.refreshSpecificationReviewPullRequest(ctx, registration, runStore, *run); err != nil {
			return AgentResult{}, err
		}
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		published, err := s.ensureClarificationPublication(ctx, registration, runStore, *run)
		if err != nil {
			return AgentResult{}, err
		}
		*run = published
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// refreshSpecificationReviewPullRequest updates only the generated review
// subsection of the tracked draft PR, preserving all human-authored text.
func (s *Service) refreshSpecificationReviewPullRequest(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) error {
	client := s.pullRequestClient()
	if client == nil {
		return errors.New("GitHub client does not support pull-request operations")
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return fmt.Errorf("decode specification packet for review PR projection: %w", err)
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	existing, err := client.FindPullRequest(ctx, repository, run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return fmt.Errorf("find tracked pull request for review projection: %w", err)
	}
	if existing.Number == 0 || existing.Number != run.PullRequestNumber {
		return fmt.Errorf("tracked pull request #%d was not found for review projection", run.PullRequestNumber)
	}
	body := mergeGeneratedReviewSection(existing.Body, generatedReviewSection(run))
	updateRequest := github.PullRequestRequest{
		Title:      defaultString(existing.Title, defaultString(packet.Issue.Title, fmt.Sprintf("Issue #%d", packet.Issue.Number))),
		Body:       body,
		HeadBranch: run.Branch,
		BaseBranch: packet.RepositoryConfig.TargetBranch,
		Draft:      true,
	}
	updated := existing
	if _, journaled := runStore.(PendingEffectStore); journaled {
		if err := s.updatePullRequestWithEffect(ctx, runStore, run.ID, client, repository, existing.Number, updateRequest); err != nil {
			return fmt.Errorf("update pull request with specification review: %w", err)
		}
		updated.Body = updateRequest.Body
		updated.Title = updateRequest.Title
		updated.Draft = updateRequest.Draft
	} else {
		updated, err = client.UpdatePullRequest(ctx, repository, existing.Number, updateRequest)
	}
	if err != nil {
		return fmt.Errorf("update pull request with specification review: %w", err)
	}
	if updated.Number <= 0 {
		return errors.New("review pull-request update returned no pull-request identity")
	}
	return nil
}
