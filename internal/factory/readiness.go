package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// finalizeReviewReadiness performs the final target synchronization, explicit
// PR readiness transition, durable ready state, and operator notification.
// A target change returns the run to checks so every gate and reviewer starts
// again from the new exact checkpoint.
func (s *Service) finalizeReviewReadiness(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return run, fmt.Errorf("decode specification packet before readiness: %w", err)
	}
	client := s.pullRequestClient()
	if client == nil {
		return run, errors.New("GitHub client does not support pull-request operations")
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	existing, err := client.FindPullRequest(ctx, repository, run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return run, fmt.Errorf("find pull request before readiness: %w", err)
	}
	if existing.Number == 0 || existing.Number != run.PullRequestNumber {
		return run, fmt.Errorf("tracked pull request #%d was not found before readiness", run.PullRequestNumber)
	}
	if existing.Merged || existing.State != "" && existing.State != "open" {
		return run, fmt.Errorf("pull request #%d is not open and cannot become ready", existing.Number)
	}

	if packet.RepositoryConfig.BaseSynchronization.Mode == config.BaseSynchronizationBeforeReady {
		workspace := s.gitWorkspace()
		inspector := s.worktreeInspector()
		if workspace == nil || inspector == nil {
			return run, errors.New("GitWorkspace is required for before-ready base synchronization")
		}
		before, err := inspector.Inspect(ctx, run.Worktree)
		if err != nil {
			return run, fmt.Errorf("inspect worktree before base synchronization: %w", err)
		}
		if before.HeadSHA != run.CheckpointSHA || len(before.ChangedPaths) != 0 {
			return run, errors.New("before-ready base synchronization requires the reviewed checkpoint worktree")
		}
		if err := workspace.SynchronizeBase(ctx, gitadapter.BaseSyncRequest{WorktreePath: run.Worktree, TargetBranch: packet.RepositoryConfig.BaseSynchronization.Branch}); err != nil {
			return run, fmt.Errorf("synchronize target branch before readiness: %w", err)
		}
		after, err := inspector.Inspect(ctx, run.Worktree)
		if err != nil {
			return run, fmt.Errorf("inspect worktree after base synchronization: %w", err)
		}
		if len(after.ChangedPaths) != 0 {
			return run, errors.New("base synchronization left uncommitted worktree changes")
		}
		if after.HeadSHA != run.CheckpointSHA {
			next := run
			next.CheckpointSHA = after.HeadSHA
			next.Stage = store.StageCheck
			next.Status = store.StatusActive
			next.SpecificationReview = nil
			next.StandardsReview = nil
			next.ReviewRepairPendingAttempt = 0
			next.ReviewRepairPacket = nil
			next.ReadyNotificationSent = false
			next.LifecycleReason = "target branch advanced before readiness; rerunning gates and independent reviews"
			next.Revision = run.Revision + 1
			next.UpdatedAt = s.deps.Now().UTC()
			if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
				return run, fmt.Errorf("persist target synchronization checkpoint: %w", err)
			}
			return next, nil
		}
	}

	if existing.Draft {
		updated, err := s.setPullRequestDraft(ctx, repository, existing, false)
		if err != nil {
			return run, err
		}
		existing = updated
	}
	if existing.Draft {
		return run, fmt.Errorf("pull request #%d remained draft after readiness request", existing.Number)
	}

	next := run
	next.Stage = store.StageReady
	next.Status = store.StatusActive
	next.LifecycleReason = "all configured reviews passed; pull request is ready for human merge"
	if run.Stage != store.StageReady || run.Status != store.StatusActive || run.LifecycleReason != next.LifecycleReason {
		next.ReadyNotificationSent = false
	}
	if next.Stage != run.Stage || next.Status != run.Status || next.LifecycleReason != run.LifecycleReason {
		next.Revision = run.Revision + 1
		next.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
			return run, fmt.Errorf("persist ready state: %w", err)
		}
	}
	return s.ensureReadyNotification(ctx, registration, runStore, next)
}

// setPullRequestDraft toggles readiness through the dedicated adapter when
// available and retains an update-body fallback for older test/embedding
// clients.
func (s *Service) setPullRequestDraft(ctx context.Context, repository github.Repository, existing github.PullRequest, draft bool) (github.PullRequest, error) {
	if existing.Draft == draft {
		return existing, nil
	}
	client := s.pullRequestClient()
	if draftClient, ok := client.(github.PullRequestDraftClient); ok {
		updated, err := draftClient.SetPullRequestDraft(ctx, repository, existing.Number, draft)
		if err != nil {
			return github.PullRequest{}, fmt.Errorf("set pull request #%d draft=%t: %w", existing.Number, draft, err)
		}
		if updated.Number == 0 {
			return github.PullRequest{}, fmt.Errorf("set pull request #%d draft=%t returned no pull-request identity", existing.Number, draft)
		}
		return updated, nil
	}
	updated, err := client.UpdatePullRequest(ctx, repository, existing.Number, github.PullRequestRequest{
		Title: existing.Title, Body: existing.Body, HeadBranch: existing.HeadBranch, BaseBranch: existing.BaseBranch, Draft: draft,
	})
	if err != nil {
		return github.PullRequest{}, fmt.Errorf("fallback pull-request readiness update #%d: %w", existing.Number, err)
	}
	if updated.Number == 0 {
		updated = existing
	}
	updated.Draft = draft
	return updated, nil
}

// ensureReadyNotification sends one concise cmux readiness notice after the
// ready state is durable and records its delivery in the run projection.
func (s *Service) ensureReadyNotification(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if run.ReadyNotificationSent {
		return run, nil
	}
	body := fmt.Sprintf("%s pull request #%d is ready for human review: %s", run.ID, run.PullRequestNumber, run.PullRequestURL)
	if err := s.notifyWorkspace(ctx, registration, "factory pull request ready", body); err != nil {
		return run, fmt.Errorf("notify pull-request readiness: %w", err)
	}
	run.ReadyNotificationSent = true
	run.UpdatedAt = s.deps.Now().UTC()
	if err := saveRunWithRetry(ctx, runStore, run); err != nil {
		return run, fmt.Errorf("persist pull-request readiness notification: %w", err)
	}
	return run, nil
}
