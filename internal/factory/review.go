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
	// StandardsReviewStatusContext is the stable exact-SHA status context used
	// by every documented-standards review invocation.
	StandardsReviewStatusContext = "factory/review/standards"
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
	return reviewContextForRole(ctx, run, runStore, workflow.RoleSpecificationReview)
}

// reviewContextForRole assembles one isolated review input. It includes prior
// findings from the same role only; a concurrent reviewer never receives the
// other reviewer's conclusions.
func reviewContextForRole(ctx context.Context, run store.Run, runStore RunStore, role string) (*prompt.ReviewContext, error) {
	contextValue := &prompt.ReviewContext{
		CheckpointSHA: run.CheckpointSHA,
	}
	if role == workflow.RoleStandardsReview {
		// The exemption is a deliberate standards-review input, not inherited
		// test-session context. No test or implementation handoff crosses the
		// reviewer boundary.
		contextValue.TestExemption = run.TestExemption
	}
	if review := reviewResultForRole(run, role); review != nil && review.CheckpointSHA == run.CheckpointSHA {
		contextValue.PriorFindings = append([]store.ReviewFinding(nil), review.Findings...)
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
	return s.ensureReviewStartForRole(ctx, run, workflow.RoleSpecificationReview)
}

// ensureReviewStartForRole verifies one reviewer receives a clean immutable
// checkpoint. A second reviewer may join an already active review round.
func (s *Service) ensureReviewStartForRole(ctx context.Context, run store.Run, role string) error {
	if run.Stage != store.StageDraftPR && run.Stage != store.StageReview {
		return fmt.Errorf("%s requires draft_pr or review stage, not %q", role, run.Stage)
	}
	if run.PullRequestNumber <= 0 {
		return fmt.Errorf("%s requires a tracked pull request", role)
	}
	if !github.ValidCommitSHA(run.CheckpointSHA) {
		return fmt.Errorf("%s requires a valid checkpoint SHA", role)
	}
	base := run.BaseCheckpointSHA
	if base == "" {
		base = run.CheckpointSHA
	}
	if !github.ValidCommitSHA(base) {
		return fmt.Errorf("%s requires a valid base checkpoint SHA", role)
	}
	if review := reviewResultForRole(run, role); review != nil && review.CheckpointSHA == run.CheckpointSHA {
		return fmt.Errorf("%s already has a result for this checkpoint", role)
	}
	inspector := s.worktreeInspector()
	if inspector == nil {
		return fmt.Errorf("worktree runtime is required for immutable %s", role)
	}
	state, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		return fmt.Errorf("inspect worktree before %s: %w", role, err)
	}
	if state.HeadSHA != run.CheckpointSHA {
		return fmt.Errorf("%s worktree HEAD %q does not match checkpoint %q", role, state.HeadSHA, run.CheckpointSHA)
	}
	if len(state.ChangedPaths) != 0 {
		return fmt.Errorf("%s requires a clean checkpoint worktree", role)
	}
	return nil
}

// reviewRoleConfigured reports whether a frozen packet declares a complete
// harness and non-empty model policy for one review role.
func reviewRoleConfigured(run store.Run, role string) bool {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return false
	}
	harnessName, hasHarness := packet.RepositoryConfig.RoleHarnessDefaults[role]
	models, hasModel := packet.RepositoryConfig.ModelOptions[role]
	return hasHarness && strings.TrimSpace(string(harnessName)) != "" && hasModel && len(models) > 0
}

// standardsReviewConfigured reports whether the frozen packet declares the
// standards reviewer as a runnable role. Older packets may predate the second
// reviewer; those packets retain the single-review compatibility path.
func standardsReviewConfigured(run store.Run) bool {
	return reviewRoleConfigured(run, workflow.RoleStandardsReview)
}

// concurrentReviewsConfigured reports whether both isolated review roles can
// be launched automatically from the frozen packet.
func concurrentReviewsConfigured(run store.Run) bool {
	return reviewRoleConfigured(run, workflow.RoleSpecificationReview) && standardsReviewConfigured(run)
}

// reviewRoundComplete reports whether every reviewer configured for the frozen
// packet has produced an exact-checkpoint result. A result from another
// checkpoint never completes the round.
func reviewRoundComplete(run store.Run) bool {
	if reviewRoleConfigured(run, workflow.RoleSpecificationReview) {
		specification := reviewResultForRole(run, workflow.RoleSpecificationReview)
		if specification == nil || specification.CheckpointSHA != run.CheckpointSHA {
			return false
		}
	}
	if !standardsReviewConfigured(run) {
		return true
	}
	standards := reviewResultForRole(run, workflow.RoleStandardsReview)
	return standards != nil && standards.CheckpointSHA == run.CheckpointSHA
}

// captureReviewDiff obtains the exact base-to-checkpoint diff inside the
// pinned worker after its review role mount has been established.
func (s *Service) captureReviewDiff(ctx context.Context, run store.Run, invocation store.Invocation) (string, error) {
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
		WorkerID:          workerIDForInvocation(invocation),
		Command:           fmt.Sprintf("git diff --no-ext-diff --binary --unified=80 %s %s -- .", base, run.CheckpointSHA),
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              invocation.Role,
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
	return s.publishReviewStatus(ctx, registration, runStore, run, workflow.RoleSpecificationReview, state, description)
}

// publishReviewStatus publishes one role-owned exact-SHA review status. The
// context is stable per reviewer role so the two results never overwrite each
// other on GitHub.
func (s *Service) publishReviewStatus(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, role string, state github.CommitStatusState, description string) error {
	if s.deps.CommitStatuses == nil {
		return fmt.Errorf("commit-status publisher is required for %s", role)
	}
	if !github.ValidCommitSHA(run.CheckpointSHA) {
		return fmt.Errorf("%s requires a valid checkpoint SHA", role)
	}
	statusContext := reviewStatusContext(role)
	if statusContext == "" {
		return fmt.Errorf("unsupported review role %q", role)
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
		Context:     statusContext,
		Description: description,
		TargetURL:   run.PullRequestURL,
	})
}

// reviewStatusContext maps a declared review role to its stable GitHub status
// context.
func reviewStatusContext(role string) string {
	switch role {
	case workflow.RoleSpecificationReview:
		return SpecificationReviewStatusContext
	case workflow.RoleStandardsReview:
		return StandardsReviewStatusContext
	default:
		return ""
	}
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

// reviewFindingBlocksForRole applies the role-specific review boundary. The
// standards axis can block only on a concrete documented-standards finding;
// its heuristic or cross-axis observations remain advisory.
func reviewFindingBlocksForRole(role string, finding store.ReviewFinding) bool {
	if role == workflow.RoleStandardsReview && finding.Category != string(report.ReviewCategoryDocumentedStandards) {
		return false
	}
	return reviewFindingBlocks(finding)
}

// reviewFindingCounts separates concrete gating violations from visible
// advisories for every supervision surface.
func reviewFindingCounts(findings []store.ReviewFinding) (blocking, advisory int) {
	return reviewFindingCountsForRole("", findings)
}

// reviewFindingCountsForRole separates blocking and advisory findings under a
// particular review axis for status and pull-request projections.
func reviewFindingCountsForRole(role string, findings []store.ReviewFinding) (blocking, advisory int) {
	for _, finding := range findings {
		if reviewFindingBlocksForRole(role, finding) {
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
	return reviewHasBlockingFindingForRole("", findings)
}

// reviewHasBlockingFindingForRole reports whether one role's result contains a
// finding permitted to gate readiness on that role's review axis.
func reviewHasBlockingFindingForRole(role string, findings []store.ReviewFinding) bool {
	for _, finding := range findings {
		if reviewFindingBlocksForRole(role, finding) {
			return true
		}
	}
	return false
}

// specificationReviewFromReport converts a validated completed review into a
// durable exact-checkpoint projection.
func specificationReviewFromReport(value report.ReviewHandoff) *store.SpecificationReview {
	return reviewResultFromReport(value)
}

// reviewResultFromReport converts a validated review handoff into a durable
// exact-checkpoint result shared by both reviewer roles.
func reviewResultFromReport(value report.ReviewHandoff) *store.ReviewResult {
	result := &store.ReviewResult{
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

// reviewResultForRole returns the durable result owned by one reviewer role.
func reviewResultForRole(run store.Run, role string) *store.ReviewResult {
	switch role {
	case workflow.RoleSpecificationReview:
		return run.SpecificationReview
	case workflow.RoleStandardsReview:
		return run.StandardsReview
	default:
		return nil
	}
}

// setReviewResultForRole stores a review result in the role-specific durable
// projection without allowing one reviewer to overwrite the other.
func setReviewResultForRole(run *store.Run, role string, result *store.ReviewResult) error {
	if run == nil {
		return errors.New("review result run is required")
	}
	switch role {
	case workflow.RoleSpecificationReview:
		run.SpecificationReview = result
	case workflow.RoleStandardsReview:
		run.StandardsReview = result
	default:
		return fmt.Errorf("unsupported review role %q", role)
	}
	return nil
}

// acceptSpecificationReviewReport preserves the original specification-review
// seam while routing through the concurrent review-round implementation.
func (s *Service) acceptSpecificationReviewReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	return s.acceptReviewReport(ctx, registration, runStore, run, invocation, value)
}

// acceptReviewReport applies one reviewer result to the serialized review
// round. A successful first result leaves the run in review until every
// configured reviewer has produced a result for the same checkpoint.
func (s *Service) acceptReviewReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if run.CheckpointSHA == "" || !github.ValidCommitSHA(run.CheckpointSHA) {
		return AgentResult{}, errors.New("cannot accept review without a valid immutable checkpoint")
	}
	roleDefinition, err := roleDefinitionForInvocation(*invocation)
	if err != nil {
		return AgentResult{}, err
	}
	previous := *run
	humanReviewWaiting := run.Stage == store.StageReview && run.Status == store.StatusWaitingForHuman && (len(run.PendingQuestions) > 0 || strings.Contains(run.LifecycleReason, "requested clarification") || strings.Contains(run.LifecycleReason, "cannot proceed"))
	statusState := github.CommitStatusError
	statusDescription := fmt.Sprintf("%s requires human disposition", invocation.Role)
	switch value.Outcome {
	case report.OutcomeCompleted:
		review := reviewResultFromReport(*value.ReviewHandoff)
		if err := setReviewResultForRole(run, invocation.Role, review); err != nil {
			return AgentResult{}, err
		}
		blocking := reviewHasBlockingResult(*run)
		complete := reviewRoundComplete(*run)
		if blocking && complete {
			run.Stage = store.StageReview
			run.Status = store.StatusActive
			run.LifecycleReason = fmt.Sprintf("review round has blocking violations; routing repair (%s)", invocation.Role)
			statusState = github.CommitStatusFailure
			if !reviewHasBlockingFindingForRole(invocation.Role, review.Findings) {
				statusState = github.CommitStatusSuccess
				statusDescription = fmt.Sprintf("%s passed; another review has blocking findings", invocation.Role)
			} else {
				statusDescription = fmt.Sprintf("%s found blocking findings", invocation.Role)
			}
		} else if blocking {
			run.Stage = store.StageReview
			run.Status = store.StatusActive
			run.LifecycleReason = fmt.Sprintf("%s found blocking findings; waiting for the other review", invocation.Role)
			if reviewHasBlockingFindingForRole(invocation.Role, review.Findings) {
				statusState = github.CommitStatusFailure
				statusDescription = fmt.Sprintf("%s found blocking findings; waiting for the other review", invocation.Role)
			} else {
				statusState = github.CommitStatusSuccess
				statusDescription = fmt.Sprintf("%s passed; another review has blocking findings", invocation.Role)
			}
		} else if humanReviewWaiting {
			run.Stage = store.StageReview
			run.Status = store.StatusWaitingForHuman
			run.LifecycleReason = fmt.Sprintf("review round remains waiting for human disposition after %s", invocation.Role)
			statusState = github.CommitStatusSuccess
			statusDescription = fmt.Sprintf("%s passed; another review still needs human disposition", invocation.Role)
		} else if reviewRoundComplete(*run) {
			// Keep readiness provisional until the coordinator has revalidated
			// final-SHA gates, synchronized the target when configured, removed
			// the draft flag, and persisted the ready projection.
			run.Stage = store.StageReview
			run.Status = store.StatusActive
			run.LifecycleReason = "all configured reviews passed; finalizing pull-request readiness"
			statusState = github.CommitStatusSuccess
			statusDescription = fmt.Sprintf("%s passed; review round complete", invocation.Role)
		} else {
			run.Stage = store.StageReview
			run.Status = store.StatusActive
			run.LifecycleReason = fmt.Sprintf("%s passed; waiting for the other review", invocation.Role)
			statusState = github.CommitStatusSuccess
			statusDescription = fmt.Sprintf("%s passed; %d advisory findings", invocation.Role, len(review.Findings))
		}
		run.PendingQuestions = nil
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	case report.OutcomeNeedsClarification:
		if err := setReviewResultForRole(run, invocation.Role, nil); err != nil {
			return AgentResult{}, err
		}
		run.Stage = store.StageReview
		run.Status = store.StatusWaitingForHuman
		run.LifecycleReason = fmt.Sprintf("%s requested clarification", invocation.Role)
		run.PendingQuestions = pendingQuestionsFromReport(value.Questions)
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	case report.OutcomeCannotProceed:
		if err := setReviewResultForRole(run, invocation.Role, nil); err != nil {
			return AgentResult{}, err
		}
		run.Stage = store.StageReview
		run.Status = store.StatusWaitingForHuman
		run.LifecycleReason = fmt.Sprintf("%s cannot proceed", invocation.Role)
		run.PendingQuestions = nil
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
	default:
		return AgentResult{}, fmt.Errorf("unsupported %s outcome %q", invocation.Role, value.Outcome)
	}
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.publishReviewStatus(ctx, registration, runStore, *run, invocation.Role, statusState, statusDescription); err != nil {
		return AgentResult{}, fmt.Errorf("publish %s status: %w", invocation.Role, err)
	}
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist %s state: %w", invocation.Role, err)
	}
	if value.Outcome == report.OutcomeCompleted && roleDefinition.Kind == workflow.RoleKindReview {
		if err := s.refreshSpecificationReviewPullRequest(ctx, registration, runStore, *run); err != nil {
			return AgentResult{}, fmt.Errorf("refresh pull request after %s: %w", invocation.Role, err)
		}
		if reviewRoundComplete(*run) {
			if reviewHasBlockingResult(*run) {
				repair, repairErr := s.routeReviewRepair(ctx, registration, runStore, *run)
				if repairErr != nil {
					return AgentResult{}, repairErr
				}
				*run = repair.Run
			} else {
				ready, readyErr := s.finalizeReviewReadiness(ctx, registration, runStore, *run)
				if readyErr != nil {
					return AgentResult{}, readyErr
				}
				*run = ready
			}
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
