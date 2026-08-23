package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// LifecycleRequest selects the run whose GitHub lifecycle should be observed.
// An empty RunID selects the most recently persisted run.
type LifecycleRequest struct {
	// RunID identifies the run to observe.
	RunID string
}

// LifecycleOutcome identifies the terminal transition, or the absence of one,
// produced by one lifecycle observation.
type LifecycleOutcome string

const (
	// LifecycleUnchanged means GitHub did not request a terminal transition.
	LifecycleUnchanged LifecycleOutcome = "unchanged"
	// LifecycleCompleted means the tracked pull request was merged.
	LifecycleCompleted LifecycleOutcome = "completed"
	// LifecycleCancelled means the issue or an unmerged pull request was closed.
	LifecycleCancelled LifecycleOutcome = "cancelled"
)

// LifecycleResult reports one idempotent lifecycle observation and the latest
// persisted run projection.
type LifecycleResult struct {
	// Outcome identifies the observed lifecycle decision.
	Outcome LifecycleOutcome
	// Run is the run after any terminal transition.
	Run store.Run
	// Reason is the coordinator-owned explanation for a cancellation or merge.
	Reason string
}

// PollLifecycle observes GitHub lifecycle state once and applies the required
// terminal transition. It never removes a branch, worktree, worker, or run
// artifacts, so cleanup and retention remain explicit later operations.
func (s *Service) PollLifecycle(ctx context.Context, request LifecycleRequest) (LifecycleResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	registration, runStore, run, err := s.openCommandRunStore(ctx)
	if err != nil {
		return LifecycleResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return LifecycleResult{Outcome: LifecycleUnchanged}, nil
	}
	if request.RunID != "" && request.RunID != run.ID {
		return LifecycleResult{}, fmt.Errorf("latest run is %s, not %s", run.ID, request.RunID)
	}
	return s.observeLifecycle(ctx, registration, runStore, run)
}

// ObserveLifecycle is an explicit spelling of PollLifecycle for callers that
// perform one-shot lifecycle checks outside the regular command poller.
func (s *Service) ObserveLifecycle(ctx context.Context, request LifecycleRequest) (LifecycleResult, error) {
	return s.PollLifecycle(ctx, request)
}

// observeLifecycle applies one lifecycle decision against an already-open
// command store. Terminal runs are returned without repeating external effects.
func (s *Service) observeLifecycle(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) (LifecycleResult, error) {
	if run == nil {
		return LifecycleResult{Outcome: LifecycleUnchanged}, nil
	}
	if store.IsTerminalStatus(run.Status) {
		return LifecycleResult{Outcome: LifecycleUnchanged, Run: *run, Reason: run.LifecycleReason}, nil
	}

	repository := commandRepository(registration)
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("read issue lifecycle for run %q: %w", run.ID, err)
	}
	pullRequest, hasPullRequest, err := s.trackedPullRequest(ctx, registration, *run)
	if err != nil {
		return LifecycleResult{}, err
	}

	// GitHub reports a merged pull request as closed, so merge detection must
	// happen before either ordinary closed-state cancellation branch.
	if hasPullRequest && pullRequest.Merged {
		reason := fmt.Sprintf("pull request #%d merged", pullRequest.Number)
		next := *run
		next.Stage = store.StageReady
		next.Status = store.StatusComplete
		next.MergeCommitSHA = pullRequest.MergeCommitSHA
		next.LifecycleReason = reason
		next.UpdatedAt = s.deps.Now().UTC()
		updated, transitionErr := s.transitionTerminal(ctx, registration, runStore, *run, next, issue)
		return LifecycleResult{Outcome: LifecycleCompleted, Run: updated, Reason: reason}, transitionErr
	}

	reason := ""
	switch {
	case hasPullRequest && strings.EqualFold(strings.TrimSpace(pullRequest.State), "closed"):
		reason = fmt.Sprintf("pull request #%d closed without merging", pullRequest.Number)
	case strings.EqualFold(strings.TrimSpace(issue.State), "closed"):
		reason = fmt.Sprintf("issue #%d closed", run.IssueNumber)
	default:
		return LifecycleResult{Outcome: LifecycleUnchanged, Run: *run}, nil
	}
	next := *run
	next.Status = store.StatusCancelled
	next.LifecycleReason = reason
	next.MergeCommitSHA = ""
	next.UpdatedAt = s.deps.Now().UTC()
	updated, transitionErr := s.transitionTerminal(ctx, registration, runStore, *run, next, issue)
	return LifecycleResult{Outcome: LifecycleCancelled, Run: updated, Reason: reason}, transitionErr
}

// trackedPullRequest loads the PR found by the run's exact branch and frozen
// target branch. A missing PR is valid before draft-PR creation.
func (s *Service) trackedPullRequest(ctx context.Context, registration config.RepositoryRegistration, run store.Run) (github.PullRequest, bool, error) {
	if run.PullRequestNumber == 0 {
		return github.PullRequest{}, false, nil
	}
	client := s.pullRequestClient()
	if client == nil {
		return github.PullRequest{}, false, errors.New("GitHub pull-request client is required to observe a tracked pull request")
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return github.PullRequest{}, false, fmt.Errorf("decode specification packet for lifecycle observation: %w", err)
	}
	if strings.TrimSpace(packet.RepositoryConfig.TargetBranch) == "" {
		return github.PullRequest{}, false, errors.New("tracked run has no frozen pull-request target branch")
	}
	pullRequest, err := client.FindPullRequest(ctx, commandRepository(registration), run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return github.PullRequest{}, false, fmt.Errorf("observe pull request #%d: %w", run.PullRequestNumber, err)
	}
	if pullRequest.Number == 0 {
		return github.PullRequest{}, false, nil
	}
	if pullRequest.Number != run.PullRequestNumber {
		return github.PullRequest{}, false, fmt.Errorf("tracked pull request changed from #%d to #%d", run.PullRequestNumber, pullRequest.Number)
	}
	return pullRequest, true, nil
}

// retryTargetIsOpen confirms that a cancelled run has an explicitly reopened
// GitHub target before the retry command reactivates its persisted state.
func (s *Service) retryTargetIsOpen(ctx context.Context, registration config.RepositoryRegistration, run store.Run, issue github.Issue) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		return true, nil
	}
	pullRequest, found, err := s.trackedPullRequest(ctx, registration, run)
	if err != nil {
		return false, err
	}
	return found && !pullRequest.Merged && strings.EqualFold(strings.TrimSpace(pullRequest.State), "open"), nil
}

// transitionTerminal stops the worker, projects the terminal state to GitHub,
// persists it, and raises one operator notification. It deliberately leaves
// terminal surfaces, sessions, branches, worktrees, and logs in place.
func (s *Service) transitionTerminal(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run, issue github.Issue) (store.Run, error) {
	if store.IsTerminalStatus(previous.Status) {
		return previous, nil
	}
	if err := s.stopRunWorker(ctx, previous.ID); err != nil {
		return next, err
	}
	repository := commandRepository(registration)
	if next.StatusCommentID == "" {
		comment, err := s.deps.GitHub.FindStatusComment(ctx, repository, next.IssueNumber, statusCommentMarker(next.ID))
		if err != nil {
			return next, fmt.Errorf("recover status comment for lifecycle transition: %w", err)
		}
		if strings.TrimSpace(comment.ID) == "" {
			return next, errors.New("run has no recoverable status comment for lifecycle transition")
		}
		previous.StatusCommentID = comment.ID
		next.StatusCommentID = comment.ID
	}
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: repository,
		Issue:      issue,
		Previous:   previous,
		Next:       next,
	})
	if err != nil {
		return updated, err
	}
	if err := s.notifyTerminal(ctx, registration, updated); err != nil {
		return updated, err
	}
	return updated, nil
}

// stopRunWorker stops an existing worker without removing its persistent
// container, role volume, worktree, or logs.
func (s *Service) stopRunWorker(ctx context.Context, runID string) error {
	if s.deps.Worker == nil {
		return nil
	}
	if err := s.deps.Worker.Stop(ctx, runID); err != nil {
		return fmt.Errorf("stop worker for run %q: %w", runID, err)
	}
	return nil
}

// notifyTerminal sends one concise cmux notification while retaining all run
// surfaces for later inspection or explicit resume.
func (s *Service) notifyTerminal(ctx context.Context, registration config.RepositoryRegistration, run store.Run) error {
	terminalRuntime := s.deps.Terminal
	if terminalRuntime == nil {
		var err error
		terminalRuntime, _, err = s.ensureAgentRuntime(registration.Cmux.SocketPath)
		if err != nil {
			return fmt.Errorf("ensure terminal runtime for lifecycle notification: %w", err)
		}
	}
	control, err := terminalRuntime.EnsureControlWorkspace(ctx, terminal.WorkspaceRequest{
		Name:             defaultString(registration.Cmux.ControlWorkspace, "factory-control"),
		Description:      "software factory coordinator",
		WorkingDirectory: registration.Path,
	})
	if err != nil {
		return fmt.Errorf("ensure control workspace for lifecycle notification: %w", err)
	}
	title := "factory run cancelled"
	body := fmt.Sprintf("%s cancelled: %s", run.ID, safeStatusCommentValue(run.LifecycleReason))
	if run.Status == store.StatusComplete {
		title = "factory run completed"
		body = fmt.Sprintf("%s completed after pull request #%d merged", run.ID, run.PullRequestNumber)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{WorkspaceID: control.ID, Title: title, Body: body}); err != nil {
		return fmt.Errorf("notify lifecycle transition: %w", err)
	}
	return nil
}
