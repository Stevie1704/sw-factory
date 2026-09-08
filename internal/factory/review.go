package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// SpecificationReviewStatusContext is the stable exact-SHA status context
	// used by every specification-review invocation.
	SpecificationReviewStatusContext = "factory/review/specification"
	// StandardsReviewStatusContext is the stable exact-SHA status context used
	// by every documented-standards review invocation.
	StandardsReviewStatusContext = "factory/review/standards"
	// reviewDiffContextLines is the context width of the captured diff. A review
	// role has the checkpoint worktree mounted read-only, so it can open any
	// file it needs; the diff only has to make each hunk judgeable in place.
	// The artifact is paged by the reviewer, so its size does not affect packet
	// size.
	reviewDiffContextLines = 10
)

// reviewAxisRoles lists the reviewer roles that own a per-axis durable result
// for one checkpoint, in the order their projections are read.
var reviewAxisRoles = []string{workflow.RoleSpecificationReview, workflow.RoleStandardsReview}

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
	return ensureReviewStartForRoleWithInspector(ctx, run, role, s.worktreeInspector())
}

// ensureReviewStartForRoleWithInspector verifies one reviewer receives a clean
// immutable checkpoint using only the supplied read-only worktree seam.
func ensureReviewStartForRoleWithInspector(ctx context.Context, run store.Run, role string, inspector gitadapter.WorktreeInspector) error {
	if run.Stage != store.StageDraftPR && run.Stage != store.StageReview {
		return fmt.Errorf("%s requires draft_pr or review stage, not %q", role, run.Stage)
	}
	if run.PullRequestNumber <= 0 {
		return fmt.Errorf("%s requires a tracked pull request", role)
	}
	if !github.ValidCommitSHA(run.CheckpointSHA) {
		return fmt.Errorf("%s requires a valid checkpoint SHA", role)
	}
	if !github.ValidCommitSHA(reviewDiffBase(run)) {
		return fmt.Errorf("%s requires a valid base checkpoint SHA", role)
	}
	if review := reviewResultForRole(run, role); review != nil && review.CheckpointSHA == run.CheckpointSHA {
		return fmt.Errorf("%s already has a result for this checkpoint", role)
	}
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
// packet has produced an exact-checkpoint result, regardless of disposition. A
// result from another checkpoint never completes the round.
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

// reviewDiffBase returns the immutable commit a checkpoint is reviewed against.
func reviewDiffBase(run store.Run) string {
	if run.BaseCheckpointSHA != "" {
		return run.BaseCheckpointSHA
	}
	return run.CheckpointSHA
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
// two axes are disjoint in what they may gate on: the specification axis owns
// correctness, security, and the frozen specification, while the standards
// axis owns documented repository standards. A finding that crosses into the
// other axis is a visible advisory, so neither reviewer can gate readiness on
// ground the other one owns. An empty role is a supervision projection over a
// result whose axis is not in hand and keeps the report contract's policy.
func reviewFindingBlocksForRole(role string, finding store.ReviewFinding) bool {
	switch role {
	case workflow.RoleSpecificationReview:
		if finding.Category == string(report.ReviewCategoryDocumentedStandards) {
			return false
		}
	case workflow.RoleStandardsReview:
		if finding.Category != string(report.ReviewCategoryDocumentedStandards) {
			return false
		}
		// The report layer requires provenance for current prompts. Recheck a
		// present source class here because durable legacy findings predate that
		// field, while a current heuristic must not masquerade as a documented
		// rule.
		evidence := strings.TrimSpace(finding.Evidence)
		if strings.HasPrefix(evidence, "source=heuristic:") {
			return false
		}
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

// reviewResultProjectionFromReport retains every durable review-axis field,
// including an incomplete disposition, its operator-visible reason, and any
// findings established before the reviewer stopped.
func reviewResultProjectionFromReport(value report.Report, checkpoint string) *store.ReviewResult {
	result := &store.ReviewResult{
		CheckpointSHA: checkpoint,
		Outcome:       string(value.Outcome),
		Summary:       value.Summary,
		Findings:      make([]store.ReviewFinding, 0),
	}
	if value.ReviewHandoff != nil {
		result.CheckpointSHA = value.ReviewHandoff.ReviewedSHA
		result.Findings = make([]store.ReviewFinding, 0, len(value.ReviewHandoff.Findings))
		for _, finding := range value.ReviewHandoff.Findings {
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
	}
	if len(value.Questions) > 0 {
		result.Questions = make([]store.ReviewQuestion, 0, len(value.Questions))
		for _, question := range value.Questions {
			result.Questions = append(result.Questions, store.ReviewQuestion{
				ID:     question.ID,
				Prompt: question.Prompt,
			})
		}
	}
	if len(value.Evidence) > 0 {
		result.Evidence = make([]store.ReviewEvidence, 0, len(value.Evidence))
		for _, evidence := range value.Evidence {
			result.Evidence = append(result.Evidence, store.ReviewEvidence{
				Kind:   evidence.Kind,
				Detail: evidence.Detail,
			})
		}
	}
	return result
}

// effectiveReviewOutcome returns the current disposition while treating an
// empty outcome as the completed value used by pre-disposition store rows.
func effectiveReviewOutcome(review *store.ReviewResult) string {
	if review == nil || review.Outcome == "" {
		return string(report.OutcomeCompleted)
	}
	return review.Outcome
}

// reviewIsIncomplete reports whether a durable review result still requires a
// human disposition rather than having finished its assignment.
func reviewIsIncomplete(review *store.ReviewResult) bool {
	return effectiveReviewOutcome(review) != string(report.OutcomeCompleted)
}

// checkpointReviewForRole returns the durable result owned by one reviewer
// role only when it was recorded against the run's exact checkpoint.
func checkpointReviewForRole(run store.Run, role string) *store.ReviewResult {
	review := reviewResultForRole(run, role)
	if review == nil || review.CheckpointSHA != run.CheckpointSHA {
		return nil
	}
	return review
}

// reviewHasIncompleteResult reports whether any exact-checkpoint reviewer
// result still requires a human disposition. It deliberately distinguishes a
// durable incomplete result without findings from a completed clean result.
func reviewHasIncompleteResult(run store.Run) bool {
	for _, role := range reviewAxisRoles {
		review := checkpointReviewForRole(run, role)
		if review != nil && reviewIsIncomplete(review) {
			return true
		}
	}
	return false
}

// reviewQuestionsForRun rebuilds the operational question projection from
// each exact-checkpoint review result so a later axis cannot erase an earlier
// axis's clarification request.
func reviewQuestionsForRun(run store.Run) []store.PendingQuestion {
	var questions []store.PendingQuestion
	for _, role := range reviewAxisRoles {
		review := checkpointReviewForRole(run, role)
		if review == nil {
			continue
		}
		for _, question := range review.Questions {
			// Review questions are independent per axis. Namespace their
			// flattened command IDs so two valid reports may use the same local
			// identifier without making the second reviewer unfinishable.
			questions = append(questions, store.PendingQuestion{
				ID:     role + "." + question.ID,
				Prompt: question.Prompt,
			})
		}
	}
	return questions
}

// reviewWaitingReason identifies the bounded review disposition that keeps a
// complete round from becoming ready. Detailed questions and evidence remain
// in the per-axis projection and supervision surfaces.
func reviewWaitingReason(run store.Run) string {
	for _, role := range reviewAxisRoles {
		review := checkpointReviewForRole(run, role)
		if review != nil && reviewIsIncomplete(review) {
			return fmt.Sprintf("%s reported %s; human disposition required", role, effectiveReviewOutcome(review))
		}
	}
	return "review round remains waiting for human disposition"
}

// reviewProjectionDetails renders the bounded metadata retained with one
// review result. The same lines feed the issue status and pull-request
// projections so an incomplete axis cannot disappear from either surface.
func reviewProjectionDetails(review store.ReviewResult) []string {
	lines := []string{fmt.Sprintf("- disposition: `%s`", safeStatusCommentValue(effectiveReviewOutcome(&review)))}
	if strings.TrimSpace(review.Summary) != "" {
		lines = append(lines, fmt.Sprintf("- summary: %s", safeStatusCommentValue(review.Summary)))
	}
	for _, question := range review.Questions {
		lines = append(lines, fmt.Sprintf("- question `%s`: %s", safeStatusCommentValue(question.ID), safeStatusCommentValue(question.Prompt)))
	}
	for _, evidence := range review.Evidence {
		lines = append(lines, fmt.Sprintf("- evidence `%s`: %s", safeStatusCommentValue(evidence.Kind), safeStatusCommentValue(evidence.Detail)))
	}
	return lines
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

// applyReviewResultProjection records the exact-checkpoint result before an
// acceptance effect crosses its restart boundary. The effect payload therefore
// cannot replay a terminal reviewer invocation while losing its findings.
func applyReviewResultProjection(run *store.Run, role string, value report.Report) error {
	if err := setReviewResultForRole(run, role, reviewResultProjectionFromReport(value, run.CheckpointSHA)); err != nil {
		return err
	}
	run.Stage = store.StageReview
	run.PendingQuestions = reviewQuestionsForRun(*run)
	run.ClarificationCommentID = ""
	run.ClarificationNotificationSent = false
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
	if roleDefinition.Kind != workflow.RoleKindReview {
		return AgentResult{}, fmt.Errorf("invocation %q is not a review role", invocation.ID)
	}
	previous := *run
	if err := applyReviewResultProjection(run, invocation.Role, value); err != nil {
		return AgentResult{}, err
	}
	review := reviewResultForRole(*run, invocation.Role)
	blocking := reviewHasBlockingResult(*run)
	complete := reviewRoundComplete(*run)
	incomplete := reviewHasIncompleteResult(*run)
	ownBlocking := reviewHasBlockingFindingForRole(invocation.Role, review.Findings)
	statusState := github.CommitStatusSuccess
	statusDescription := fmt.Sprintf("%s passed; %d advisory findings", invocation.Role, len(review.Findings))
	switch {
	case complete && blocking:
		run.Status = store.StatusActive
		run.LifecycleReason = fmt.Sprintf("review round has blocking violations; routing repair (%s)", invocation.Role)
		if ownBlocking {
			statusState = github.CommitStatusFailure
			statusDescription = fmt.Sprintf("%s found blocking findings", invocation.Role)
		} else {
			statusDescription = fmt.Sprintf("%s passed; another review has blocking findings", invocation.Role)
		}
	case blocking:
		run.Status = store.StatusActive
		if ownBlocking {
			run.LifecycleReason = fmt.Sprintf("%s reported %s with blocking findings; waiting for the other review", invocation.Role, effectiveReviewOutcome(review))
			statusState = github.CommitStatusFailure
			statusDescription = fmt.Sprintf("%s reported %s with blocking findings; waiting for the other review", invocation.Role, effectiveReviewOutcome(review))
		} else {
			run.LifecycleReason = fmt.Sprintf("%s reported %s; another review has blocking findings", invocation.Role, effectiveReviewOutcome(review))
			statusDescription = fmt.Sprintf("%s reported %s; another review has blocking findings", invocation.Role, effectiveReviewOutcome(review))
		}
	case incomplete:
		run.Status = store.StatusWaitingForHuman
		run.LifecycleReason = reviewWaitingReason(*run)
		if !reviewIsIncomplete(review) {
			statusDescription = fmt.Sprintf("%s passed; another review still needs human disposition", invocation.Role)
		} else {
			statusState = github.CommitStatusError
			statusDescription = fmt.Sprintf("%s reported %s; human disposition required", invocation.Role, effectiveReviewOutcome(review))
		}
	case complete:
		// Keep readiness provisional until the coordinator has revalidated
		// final-SHA gates, synchronized the target when configured, removed
		// the draft flag, and persisted the ready projection.
		run.Status = store.StatusActive
		run.LifecycleReason = "all configured reviews passed; finalizing pull-request readiness"
		statusDescription = fmt.Sprintf("%s passed; review round complete", invocation.Role)
	default:
		run.Status = store.StatusActive
		run.LifecycleReason = fmt.Sprintf("%s passed; waiting for the other review", invocation.Role)
		statusDescription = fmt.Sprintf("%s passed; waiting for the other review", invocation.Role)
	}
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.publishReviewStatus(ctx, registration, runStore, *run, invocation.Role, statusState, statusDescription); err != nil {
		return AgentResult{}, fmt.Errorf("publish %s status: %w", invocation.Role, err)
	}
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist %s state: %w", invocation.Role, err)
	}
	if err := s.refreshSpecificationReviewPullRequest(ctx, registration, runStore, *run); err != nil {
		return AgentResult{}, fmt.Errorf("refresh pull request after %s: %w", invocation.Role, err)
	}
	if complete {
		if blocking {
			repair, repairErr := s.routeReviewRepair(ctx, registration, runStore, *run)
			if repairErr != nil {
				return AgentResult{}, repairErr
			}
			*run = repair.Run
		} else if !incomplete {
			ready, readyErr := s.finalizeReviewReadiness(ctx, registration, runStore, *run)
			if readyErr != nil {
				return AgentResult{}, readyErr
			}
			*run = ready
		}
	}
	if len(run.PendingQuestions) > 0 {
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
