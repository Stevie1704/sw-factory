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

// observeLifecycle applies one lifecycle decision against an already-open
// command store. Terminal runs are returned without repeating external effects.
func (s *Service) observeLifecycle(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) (LifecycleResult, error) {
	if run == nil {
		return LifecycleResult{Outcome: LifecycleUnchanged}, nil
	}
	if store.IsTerminalStatus(run.Status) {
		if run.Status == store.StatusFailed {
			return LifecycleResult{Outcome: LifecycleUnchanged, Run: *run, Reason: run.LifecycleReason}, nil
		}
		updated, notificationErr := s.ensureTerminalNotification(ctx, registration, runStore, *run)
		if notificationErr != nil {
			return LifecycleResult{Outcome: LifecycleUnchanged, Run: updated, Reason: updated.LifecycleReason}, notificationErr
		}
		return LifecycleResult{Outcome: LifecycleUnchanged, Run: updated, Reason: updated.LifecycleReason}, nil
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
		if strings.TrimSpace(pullRequest.MergeCommitSHA) == "" {
			return LifecycleResult{}, fmt.Errorf("merged pull request #%d has no merge commit", pullRequest.Number)
		}
		reason := fmt.Sprintf("pull request #%d merged", pullRequest.Number)
		next := *run
		next.Stage = store.StageReady
		next.Status = store.StatusComplete
		next.MergeCommitSHA = pullRequest.MergeCommitSHA
		next.LifecycleReason = reason
		next.LifecycleNotificationSent = false
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
	next.LifecycleNotificationSent = false
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
	issueOpen := strings.EqualFold(strings.TrimSpace(issue.State), "open")
	if run.PullRequestNumber == 0 {
		return issueOpen, nil
	}
	pullRequest, found, err := s.trackedPullRequest(ctx, registration, run)
	if err != nil {
		return false, err
	}
	pullRequestOpen := found && !pullRequest.Merged && strings.EqualFold(strings.TrimSpace(pullRequest.State), "open")
	if strings.HasPrefix(strings.TrimSpace(run.LifecycleReason), "pull request #") {
		return pullRequestOpen, nil
	}
	return issueOpen, nil
}

// transitionTerminal stops the worker, projects the terminal state to GitHub,
// persists it, and raises one operator notification. It deliberately leaves
// terminal surfaces, sessions, branches, worktrees, and logs in place.
func (s *Service) transitionTerminal(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run, issue github.Issue) (store.Run, error) {
	if store.IsTerminalStatus(previous.Status) {
		return previous, nil
	}
	journaled := false
	if _, journaled = runStore.(PendingEffectStore); !journaled {
		if err := s.stopRunWorker(ctx, previous.ID); err != nil {
			return next, err
		}
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
		StopWorker: journaled,
	})
	if err != nil {
		return updated, err
	}
	return s.ensureTerminalNotification(ctx, registration, runStore, updated)
}

// ensureTerminalNotification delivers and records the one terminal cmux
// notification, including after a prior delivery failure or restart.
// It uses a durable claim table to prevent duplicate notifications when
// notification succeeds but the subsequent run save fails.
func (s *Service) ensureTerminalNotification(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if run.LifecycleNotificationSent {
		return run, nil
	}
	// Atomically claim the right to send this notification. If the claim already
	// exists, the notification was already delivered (even if the run flag wasn't
	// persisted due to a prior save failure), so skip delivery.
	claimed, err := runStore.ClaimLifecycleNotification(ctx, run.ID, run.Status)
	if err != nil {
		return run, fmt.Errorf("claim lifecycle notification: %w", err)
	}
	if !claimed {
		// Notification already delivered by a prior attempt. Update the in-memory
		// flag and persist it without re-sending the notification.
		run.LifecycleNotificationSent = true
		run.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, run); err != nil {
			return run, fmt.Errorf("persist lifecycle notification flag after skipped delivery: %w", err)
		}
		return run, nil
	}
	// Claim succeeded. Attempt notification delivery.
	if err := s.notifyTerminal(ctx, registration, run); err != nil {
		// Delivery failed. Release the claim so a future retry can attempt delivery again.
		if releaseErr := runStore.ReleaseLifecycleNotification(ctx, run.ID, run.Status); releaseErr != nil {
			return run, fmt.Errorf("notify lifecycle transition: %w (also failed to release claim: %v)", err, releaseErr)
		}
		return run, err
	}
	// Notification delivered successfully. Keep the claim (it now durably records
	// that delivery happened) and persist the flag in the run record.
	run.LifecycleNotificationSent = true
	run.UpdatedAt = s.deps.Now().UTC()
	if err := saveRunWithRetry(ctx, runStore, run); err != nil {
		// The claim table already records successful delivery, so a retry won't
		// re-send the notification even though the run flag wasn't persisted.
		return run, fmt.Errorf("persist lifecycle notification flag: %w", err)
	}
	return run, nil
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
	title := "factory run cancelled"
	body := fmt.Sprintf("%s cancelled: %s", run.ID, safeStatusCommentValue(run.LifecycleReason))
	if run.Status == store.StatusComplete {
		title = "factory run completed"
		body = fmt.Sprintf("%s completed after pull request #%d merged", run.ID, run.PullRequestNumber)
	}
	return s.notifyWorkspace(ctx, registration, title, body)
}

// notifyWorkspace sends one concise coordinator notification through the
// registered control workspace, lazily constructing the runtime when needed.
func (s *Service) notifyWorkspace(ctx context.Context, registration config.RepositoryRegistration, title, body string) error {
	terminalRuntime := s.deps.Terminal
	if terminalRuntime == nil {
		var err error
		terminalRuntime, err = s.ensureTerminalRuntime(registration.Cmux.SocketPath)
		if err != nil {
			return fmt.Errorf("ensure terminal runtime for notification: %w", err)
		}
	}
	control, err := terminalRuntime.EnsureControlWorkspace(ctx, terminal.WorkspaceRequest{
		Name:             defaultString(registration.Cmux.ControlWorkspace, "factory-control"),
		Description:      "software factory coordinator",
		WorkingDirectory: registration.Path,
	})
	if err != nil {
		return fmt.Errorf("ensure control workspace for notification: %w", err)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{WorkspaceID: control.ID, Title: title, Body: body}); err != nil {
		return fmt.Errorf("notify coordinator: %w", err)
	}
	return nil
}
