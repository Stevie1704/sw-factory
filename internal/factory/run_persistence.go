package factory

import (
	"context"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// persistAgentRunState uses the coordinator's GitHub label/comment transition
// when the claimed run has a status comment, retaining a direct-store fallback
// for legacy runs that predate that supervision record.
func (s *Service) persistAgentRunState(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run) error {
	checkpointChanged := previous.CheckpointSHA != "" && next.CheckpointSHA != previous.CheckpointSHA
	if checkpointChanged {
		if next.Revision <= previous.Revision {
			next.Revision = previous.Revision + 1
		}
		next.SpecificationReview = nil
		next.StandardsReview = nil
		if next.TestCheckpointSHA == "" || next.TestCheckpointSHA != next.CheckpointSHA {
			next.ReviewRepairPendingAttempt = 0
			next.ReviewRepairPacket = nil
		}
	}
	if next.SpecificationReview != nil && next.SpecificationReview.CheckpointSHA != next.CheckpointSHA {
		next.SpecificationReview = nil
	}
	if next.StandardsReview != nil && next.StandardsReview.CheckpointSHA != next.CheckpointSHA {
		next.StandardsReview = nil
	}
	invalidateResults := checkpointChanged
	if invalidateResults {
		_, atomicSupported := runStore.(atomicPacketTransitionStore)
		_, directSupported := runStore.(runResultInvalidator)
		invalidateResults = atomicSupported || directSupported
	}
	if next.StatusCommentID == "" || s.deps.GitHub == nil {
		if invalidateResults {
			next.UpdatedAt = s.deps.Now().UTC()
			if atomicStore, ok := runStore.(atomicPacketTransitionStore); ok {
				if err := atomicStore.SaveRunAndInvalidateResults(ctx, previous.Revision, next); err != nil {
					return fmt.Errorf("persist checkpoint state and invalidate results: %w", err)
				}
				return nil
			}
			if err := saveRunWithRetry(ctx, runStore, next); err != nil {
				return err
			}
			return invalidateRunResults(ctx, runStore, next.ID)
		}
		return saveRunWithRetry(ctx, runStore, next)
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	issue, err := s.deps.GitHub.Issue(ctx, repository, next.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue for agent state transition: %w", err)
	}
	if _, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository:        repository,
		Issue:             issue,
		Previous:          previous,
		Next:              next,
		InvalidateResults: invalidateResults,
	}); err != nil {
		return err
	}
	return nil
}
