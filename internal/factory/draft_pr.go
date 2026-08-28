package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// generatedPullRequestStart marks the coordinator-owned PR body section.
	generatedPullRequestStart = "<!-- factory-generated:start -->"
	// generatedPullRequestEnd marks the end of the coordinator-owned PR body section.
	generatedPullRequestEnd = "<!-- factory-generated:end -->"
	// generatedReviewStart marks the nested coordinator-owned review section.
	generatedReviewStart = "<!-- factory-generated:review:start -->"
	// generatedReviewEnd marks the end of the nested review section.
	generatedReviewEnd = "<!-- factory-generated:review:end -->"
)

// DraftPullRequestRequest selects the active run whose accepted implementation
// should become a draft pull request.
type DraftPullRequestRequest struct {
	// RunID identifies the active run. Empty selects the repository's only run.
	RunID string
	// Intervention is the operator-visible marker regenerated in the PR body.
	Intervention string
}

// DraftPullRequestResult contains the final run, gate results, and draft PR
// identity produced by the coordinator.
type DraftPullRequestResult struct {
	// Run is the persisted run after the draft PR transition.
	Run store.Run
	// Gates contains one result for every configured gate.
	Gates []gate.Result
	// Repair contains the bounded check-repair decision when the checkpoint
	// suite did not yet produce a draft pull request.
	Repair *CheckRepairResult
	// PullRequest is the created or recovered draft PR.
	PullRequest github.PullRequest
}

// CreateDraftPullRequest advances an accepted implementation through its host
// checkpoint, all frozen deterministic gates, first branch push, and one
// idempotent draft pull request. Repeating it regenerates only the marked
// factory section and preserves human-authored PR text.
func (s *Service) CreateDraftPullRequest(ctx context.Context, request DraftPullRequestRequest) (DraftPullRequestResult, error) {
	if strings.ContainsAny(request.Intervention, "\x00\r\n") {
		return DraftPullRequestResult{}, errors.New("draft pull request intervention must be a single line")
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return DraftPullRequestResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return DraftPullRequestResult{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return DraftPullRequestResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if err := s.ensureInvocationAttached(ctx, runStore, *run); err != nil {
		return DraftPullRequestResult{}, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return DraftPullRequestResult{}, err
	}
	if run.Stage == store.StageDraftPR {
		pullRequest, err := s.regenerateDraftPullRequest(ctx, registration, runStore, *run, packet, request.Intervention)
		if err != nil {
			return DraftPullRequestResult{Run: *run}, err
		}
		return DraftPullRequestResult{Run: *run, PullRequest: pullRequest}, nil
	}
	if run.Stage != store.StageImplementation && run.Stage != store.StageCheck {
		return DraftPullRequestResult{}, fmt.Errorf("run %q must be in implementation or check stage, not %q", run.ID, run.Stage)
	}
	if run.Stage == store.StageImplementation && run.Status != store.StatusActive {
		return DraftPullRequestResult{}, fmt.Errorf("run %q is %s; implementation must be active", run.ID, run.Status)
	}
	if run.Stage == store.StageCheck && run.Status != store.StatusActive && run.Status != store.StatusWaitingForHarness && !isRestartReconciliationPause(*run) {
		return DraftPullRequestResult{}, fmt.Errorf("run %q is %s; check evaluation must be active or waiting for harness", run.ID, run.Status)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return DraftPullRequestResult{Run: *run}, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	if activeStore, ok := runStore.(ActiveInvocationStore); ok {
		active, lookupErr := activeStore.ActiveInvocation(ctx, run.ID)
		if lookupErr != nil {
			return DraftPullRequestResult{}, fmt.Errorf("look up active implementation invocation: %w", lookupErr)
		}
		if active != nil {
			return DraftPullRequestResult{}, fmt.Errorf("run %q still has active implementation invocation %q", run.ID, active.ID)
		}
	}
	if run.Stage == store.StageCheck && isRestartReconciliationPause(*run) {
		resumed, resumeErr := s.resumeRecoveredCheck(ctx, registration, runStore, *run)
		if resumeErr != nil {
			return DraftPullRequestResult{Run: resumed}, resumeErr
		}
		*run = resumed
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return DraftPullRequestResult{}, errors.New("GitWorkspace is required to create a draft pull request")
	}
	protectedState, err := workspace.Inspect(ctx, run.Worktree)
	if err != nil {
		return DraftPullRequestResult{}, fmt.Errorf("inspect implementation worktree before checkpoint: %w", err)
	}
	if protectedState.HeadSHA != run.CheckpointSHA {
		return DraftPullRequestResult{}, fmt.Errorf("implementation worktree HEAD %q does not match checkpoint %q", protectedState.HeadSHA, run.CheckpointSHA)
	}
	if err := validateProtectedTestPaths(run.Worktree, protectedState, run.ProtectedTestPaths); err != nil {
		return DraftPullRequestResult{}, err
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return DraftPullRequestResult{}, err
	}
	issue = ensureIssueIdentity(issue, packet.Issue, run.IssueNumber)
	next := *run
	if next.CheckRepairBudget <= 0 {
		next.CheckRepairBudget = packet.RepositoryConfig.RetryLimits.CheckRepair
	}
	if run.Stage == store.StageCheck {
		if run.Status == store.StatusWaitingForHarness {
			next.Status = store.StatusActive
			next.LifecycleReason = "resuming check evaluation after infrastructure wait"
			next.UpdatedAt = s.deps.Now().UTC()
			if err := s.persistAgentRunState(ctx, registration, runStore, *run, next); err != nil {
				return DraftPullRequestResult{Run: next}, err
			}
		}
	} else {
		checkpointTransition, transitionErr := workflow.DefaultRegistry().ResolveEventTransition(run.Stage, workflow.StageEventCheckpoint)
		if transitionErr != nil {
			return DraftPullRequestResult{}, transitionErr
		}
		checkpointRequest := gitadapter.CheckpointRequest{
			RunID:        run.ID,
			WorktreePath: run.Worktree,
			ParentSHA:    run.CheckpointSHA,
			Message:      checkpointMessage(*run),
		}
		checkpoint := gitadapter.CheckpointResult{}
		checkpointErr := error(nil)
		next.SpecificationReview = nil
		next.Stage = checkpointTransition.Stage
		next.Status = checkpointTransition.Status
		next.LifecycleReason = "evaluating implementation checkpoint"
		next.Revision = run.Revision + 1
		next.UpdatedAt = s.deps.Now().UTC()
		if _, journaled := runStore.(PendingEffectStore); journaled {
			checkpoint, next, checkpointErr = s.checkpointAndPersistWithEffect(ctx, runStore, workspace, checkpointRequest, repository, issue, *run, next)
		} else {
			checkpoint, checkpointErr = workspace.CreateCheckpoint(ctx, checkpointRequest)
		}
		if checkpointErr != nil {
			return DraftPullRequestResult{}, checkpointErr
		}
		if !github.ValidCommitSHA(checkpoint.SHA) {
			return DraftPullRequestResult{}, errors.New("GitWorkspace returned an invalid implementation checkpoint SHA")
		}
		if run.CheckRepairAttempts > 0 && checkpoint.SHA == run.CheckpointSHA {
			return DraftPullRequestResult{}, fmt.Errorf("check-repair attempt %d did not produce a new checkpoint SHA", run.CheckRepairAttempts)
		}
		if _, journaled := runStore.(PendingEffectStore); !journaled {
			next.CheckpointSHA = checkpoint.SHA
			next, err = s.applyStateTransition(ctx, runStore, stateTransition{
				Repository: repository, Issue: issue, Previous: *run, Next: next,
			})
			if err != nil {
				return DraftPullRequestResult{}, err
			}
		}
	}

	if err := s.publishCheckpointBranch(ctx, runStore, workspace, next); err != nil {
		return DraftPullRequestResult{Run: next}, err
	}

	gates, gateErr := s.runConfiguredGates(ctx, registration, runStore, next, packet)
	result := DraftPullRequestResult{Run: next, Gates: gates}
	if gateErr != nil {
		repair, repairErr := s.routeCheckRepair(ctx, registration, runStore, next, packet, gates, gateErr)
		result.Run = repair.Run
		result.Repair = &repair
		return result, repairErr
	}
	state, err := workspace.Inspect(ctx, next.Worktree)
	if err != nil {
		return result, fmt.Errorf("validate checkpoint after gates: %w", err)
	}
	if state.HeadSHA != next.CheckpointSHA {
		return result, fmt.Errorf("worktree HEAD %q changed after checkpoint %q", state.HeadSHA, next.CheckpointSHA)
	}
	if len(state.ChangedPaths) != 0 {
		return result, errors.New("worktree has uncommitted changes after deterministic gates")
	}
	draftTransition, transitionErr := workflow.DefaultRegistry().ResolveEventTransition(next.Stage, workflow.StageEventDraftPullRequest)
	if transitionErr != nil {
		return result, transitionErr
	}

	pullRequests := s.pullRequestClient()
	if pullRequests == nil {
		return result, errors.New("GitHub client does not support pull-request operations")
	}
	pullRequest := github.PullRequest{}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		plannedRequest, expectedNumber, planErr := s.planDraftPullRequest(ctx, pullRequests, repository, next, packet, gates, request.Intervention)
		if planErr != nil {
			return result, planErr
		}
		next.Stage = draftTransition.Stage
		next.Status = draftTransition.Status
		next.Revision = result.Run.Revision + 1
		next.UpdatedAt = s.deps.Now().UTC()
		pullRequest, next, err = s.upsertPullRequestAndPersistWithEffect(ctx, runStore, pullRequests, repository, issue, result.Run, next, plannedRequest, expectedNumber)
	} else {
		pullRequest, err = s.upsertDraftPullRequest(ctx, pullRequests, repository, next, packet, gates, request.Intervention)
		if err != nil {
			return result, err
		}
		next.PullRequestNumber = pullRequest.Number
		next.PullRequestURL = pullRequest.URL
		next.Stage = draftTransition.Stage
		next.Status = draftTransition.Status
		next.UpdatedAt = s.deps.Now().UTC()
		next, err = s.applyStateTransition(ctx, runStore, stateTransition{
			Repository: repository, Issue: issue, Previous: result.Run, Next: next,
		})
	}
	if err != nil {
		return DraftPullRequestResult{Run: next, Gates: gates, PullRequest: pullRequest}, err
	}
	return DraftPullRequestResult{Run: next, Gates: gates, PullRequest: pullRequest}, nil
}

// publishCheckpointBranch pushes the run branch so GitHub can resolve the
// checkpoint commit. Every gate status names that exact SHA, and GitHub
// rejects a status for a commit it has never seen, so the branch must reach
// the remote before the first status is published. The push observes the
// remote head first and is therefore safe to repeat.
func (s *Service) publishCheckpointBranch(ctx context.Context, runStore RunStore, workspace gitadapter.GitWorkspace, run store.Run) error {
	request := gitadapter.PushRequest{WorktreePath: run.Worktree, Branch: run.Branch}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.pushWithEffect(ctx, runStore, run.ID, workspace, request, run.CheckpointSHA)
	}
	return workspace.Push(ctx, request)
}

// runConfiguredGates executes the frozen gate suite once and joins blocking
// failures while retaining every independent and skipped result.
func (s *Service) runConfiguredGates(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket) ([]gate.Result, error) {
	if err := s.verifyGateCheckpoint(ctx, run); err != nil {
		return nil, err
	}
	suite, suiteErr := s.runGateSuite(ctx, registration, runStore, run, packet, gate.PhaseCheckpoint, packet.RepositoryConfig.Gates)
	if persistErr := persistGateSuite(ctx, runStore, run, suite); persistErr != nil {
		suiteErr = errors.Join(suiteErr, persistErr)
	}
	return suite.Gates, suiteErr
}

// upsertDraftPullRequest finds a prior branch pull request before creating one
// and merges the generated section into its existing body when present.
func (s *Service) upsertDraftPullRequest(ctx context.Context, client github.PullRequestClient, repository github.Repository, run store.Run, packet SpecificationPacket, gates []gate.Result, intervention string) (github.PullRequest, error) {
	request, expectedNumber, err := s.planDraftPullRequest(ctx, client, repository, run, packet, gates, intervention)
	if err != nil {
		return github.PullRequest{}, err
	}
	return s.upsertPullRequestRequest(ctx, client, repository, expectedNumber, request)
}

// planDraftPullRequest reads the current branch PR once and freezes the exact
// request used by both the first mutation and a restart replay.
func (s *Service) planDraftPullRequest(ctx context.Context, client github.PullRequestClient, repository github.Repository, run store.Run, packet SpecificationPacket, gates []gate.Result, intervention string) (github.PullRequestRequest, int, error) {
	existing, err := client.FindPullRequest(ctx, repository, run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return github.PullRequestRequest{}, 0, err
	}
	body := generatedPullRequestBody(run, packet, gates, intervention)
	request := github.PullRequestRequest{
		Title:      defaultString(packet.Issue.Title, fmt.Sprintf("Issue #%d", packet.Issue.Number)),
		Body:       body,
		HeadBranch: run.Branch,
		BaseBranch: packet.RepositoryConfig.TargetBranch,
		Draft:      true,
	}
	if existing.Number > 0 {
		request.Body = mergeGeneratedPullRequestBody(existing.Body, body)
	}
	return request, existing.Number, nil
}

// regenerateDraftPullRequest refreshes a previously created PR without
// rerunning gates or pushing again. The branch lookup supplies the current
// human-authored body for preservation.
func (s *Service) regenerateDraftPullRequest(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket, intervention string) (github.PullRequest, error) {
	client := s.pullRequestClient()
	if client == nil {
		return github.PullRequest{}, errors.New("GitHub client does not support pull-request operations")
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	existing, err := client.FindPullRequest(ctx, repository, run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return github.PullRequest{}, err
	}
	if existing.Number == 0 {
		return github.PullRequest{}, fmt.Errorf("draft pull request for branch %q was not found", run.Branch)
	}
	body := mergeGeneratedPullRequestBody(existing.Body, generatedPullRequestBody(run, packet, nil, intervention))
	updateRequest := github.PullRequestRequest{
		Title: defaultString(packet.Issue.Title, fmt.Sprintf("Issue #%d", packet.Issue.Number)), Body: body,
		HeadBranch: run.Branch, BaseBranch: packet.RepositoryConfig.TargetBranch, Draft: true,
	}
	updated := existing
	if _, journaled := runStore.(PendingEffectStore); journaled {
		err = s.updatePullRequestWithEffect(ctx, runStore, run.ID, client, repository, existing.Number, updateRequest)
	} else {
		updated, err = client.UpdatePullRequest(ctx, repository, existing.Number, updateRequest)
	}
	if err != nil {
		return github.PullRequest{}, err
	}
	updated.Body = updateRequest.Body
	updated.Title = updateRequest.Title
	updated.Draft = updateRequest.Draft
	if updated.Number <= 0 {
		return github.PullRequest{}, errors.New("pull-request regeneration returned no pull-request identity")
	}
	return updated, nil
}

// checkpointMessage identifies whether the next checkpoint follows an
// accepted implementation or a completed check-repair session.
func checkpointMessage(run store.Run) string {
	if run.CheckRepairAttempts > 0 {
		return fmt.Sprintf("accepted check-repair attempt %d", run.CheckRepairAttempts)
	}
	return "accepted implementation"
}

// ensureIssueIdentity fills the fallback issue identity used by test doubles
// and GitHub adapters that return an otherwise valid issue without a number.
func ensureIssueIdentity(issue, fallback github.Issue, issueNumber int) github.Issue {
	if issue.Number == 0 {
		issue = fallback
		issue.Number = issueNumber
	}
	return issue
}

// gitWorkspace resolves the new task-oriented seam while retaining the
// foundation Worktree adapter as a compatibility fallback.
func (s *Service) gitWorkspace() gitadapter.GitWorkspace {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	workspace, _ := s.deps.Worktree.(gitadapter.GitWorkspace)
	return workspace
}

// worktreeManager resolves the task-oriented Git workspace for the existing
// claim/cleanup operations while keeping foundation adapters compatible.
func (s *Service) worktreeManager() gitadapter.WorktreeManager {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	return s.deps.Worktree
}

// worktreeInspector resolves the read-only validation seam used by report
// acceptance.
func (s *Service) worktreeInspector() gitadapter.WorktreeInspector {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	inspector, _ := s.deps.Worktree.(gitadapter.WorktreeInspector)
	return inspector
}

// pullRequestClient resolves the dedicated pull-request adapter or a GitHub
// client that implements it directly.
func (s *Service) pullRequestClient() github.PullRequestClient {
	if s.deps.PullRequests != nil {
		return s.deps.PullRequests
	}
	client, _ := s.deps.GitHub.(github.PullRequestClient)
	return client
}

// generatedPullRequestBody renders the complete coordinator-owned PR section.
// It builds the Gates section from the observed gate results rather than
// configured gates, rendering each result's actual status.
func generatedPullRequestBody(run store.Run, packet SpecificationPacket, gates []gate.Result, intervention string) string {
	if strings.TrimSpace(intervention) == "" {
		intervention = "none"
	}
	issueBody := strings.TrimSpace(packet.Issue.Body)
	if issueBody == "" {
		issueBody = "(no issue body supplied)"
	}
	gateLines := make([]string, 0, len(gates))
	for _, result := range gates {
		var status string
		if result.Skipped {
			status = "skipped"
			if strings.TrimSpace(result.SkipReason) != "" {
				status += ": " + result.SkipReason
			}
		} else {
			switch result.Status.State {
			case github.CommitStatusSuccess:
				status = "passed"
			case github.CommitStatusFailure:
				status = "failed"
			case github.CommitStatusError:
				status = "error"
			case github.CommitStatusPending:
				status = "pending"
			default:
				status = "unknown"
			}
		}
		gateName := result.GateName
		if gateName == "" {
			gateName = "(unnamed)"
		}
		gateLines = append(gateLines, fmt.Sprintf("- `%s`: %s", gateName, status))
	}
	if len(gateLines) == 0 {
		gateLines = append(gateLines, "- (none)")
	}
	policyMode := packet.RepositoryConfig.TestPolicy.Mode
	testStageDisposition := fmt.Sprintf("- test policy: `%s`\n- route: `%s`", safeStatusCommentValue(testPolicyDescription(policyMode)), safeStatusCommentValue(packet.Route.Description()))
	if independentTestStageDeclared(packet) {
		if run.TestExemption == nil {
			testStageDisposition += "\n- test-stage disposition: none"
		} else {
			testStageDisposition += fmt.Sprintf("\n- test-stage disposition: `%s` (provisional): %s", safeStatusCommentValue(run.TestExemption.Kind), safeStatusCommentValue(run.TestExemption.Justification))
		}
	}
	return fmt.Sprintf("%s\n## Factory run\n\n- run: `%s`\n- issue: #%d — %s\n- specification packet: version %d, target branch `%s`\n- checkpoint: `%s`\n- stage: `draft_pr`\n- intervention: `%s`\n%s\n\n%s\n\n### Issue summary\n\n%s\n\n### Gates\n\n%s\n\n### Control commands\n\n- `factory status`\n- `factory draft-pr --run-id %s`\n\n%s", generatedPullRequestStart, run.ID, packet.Issue.Number, defaultString(packet.Issue.Title, "(untitled)"), packet.Version, packet.RepositoryConfig.TargetBranch, run.CheckpointSHA, intervention, testStageDisposition, generatedReviewSection(run), issueBody, strings.Join(gateLines, "\n"), run.ID, generatedPullRequestEnd)
}

// generatedReviewSection renders the latest exact-checkpoint review, including
// advisory findings that never gate readiness but must remain visible.
func generatedReviewSection(run store.Run) string {
	lines := []string{generatedReviewStart, "### Specification review", ""}
	if run.SpecificationReview == nil || run.SpecificationReview.CheckpointSHA != run.CheckpointSHA {
		if run.Stage == store.StageReview {
			lines = append(lines, fmt.Sprintf("- status: pending for checkpoint `%s`", safeStatusCommentValue(run.CheckpointSHA)))
		} else {
			lines = append(lines, "- status: not run")
		}
		lines = append(lines, generatedReviewEnd)
		return strings.Join(lines, "\n")
	}
	review := run.SpecificationReview
	blockers, advisories := reviewFindingCounts(review.Findings)
	lines = append(lines,
		fmt.Sprintf("- reviewed checkpoint: `%s`", safeStatusCommentValue(review.CheckpointSHA)),
		fmt.Sprintf("- outcome: %d blocking findings, %d advisory findings", blockers, advisories),
	)
	if len(review.Findings) == 0 {
		lines = append(lines, "- findings: none")
	} else {
		for index, finding := range review.Findings {
			lines = append(lines,
				fmt.Sprintf("\n#### Finding %d — %s/%s", index+1, safeStatusCommentValue(finding.Severity), safeStatusCommentValue(finding.Category)),
				fmt.Sprintf("- location: `%s`", safeStatusCommentValue(finding.Location)),
				fmt.Sprintf("- claim: %s", safeStatusCommentValue(finding.Claim)),
				fmt.Sprintf("- evidence: %s", safeStatusCommentValue(finding.Evidence)),
				fmt.Sprintf("- suggested resolution: %s", safeStatusCommentValue(finding.SuggestedResolution)),
				fmt.Sprintf("- suggested owner: `%s`", safeStatusCommentValue(finding.SuggestedOwner)),
			)
		}
	}
	lines = append(lines, generatedReviewEnd)
	return strings.Join(lines, "\n")
}

// mergeGeneratedPullRequestBody replaces only the marked factory section and
// appends it after existing human-authored text when no marker exists.
func mergeGeneratedPullRequestBody(existing, generated string) string {
	start := strings.Index(existing, generatedPullRequestStart)
	end := strings.Index(existing, generatedPullRequestEnd)
	if start >= 0 && end >= start {
		end += len(generatedPullRequestEnd)
		return existing[:start] + generated + existing[end:]
	}
	if strings.TrimSpace(existing) == "" {
		return generated
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + generated
}

// mergeGeneratedReviewSection replaces only the nested review projection and
// preserves both human PR text and the surrounding generated sections.
func mergeGeneratedReviewSection(existing, generated string) string {
	start := strings.Index(existing, generatedReviewStart)
	end := strings.Index(existing, generatedReviewEnd)
	if start >= 0 && end >= start {
		end += len(generatedReviewEnd)
		return existing[:start] + generated + existing[end:]
	}
	if end := strings.Index(existing, generatedPullRequestEnd); end >= 0 {
		return existing[:end] + "\n\n" + generated + "\n" + existing[end:]
	}
	return mergeGeneratedPullRequestBody(existing, generated)
}
