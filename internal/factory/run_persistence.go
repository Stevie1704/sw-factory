package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// applyStateTransition performs the shared label/comment/store transition for
// both the claim path and later state changes. It retries a transient store
// failure once and compensates external changes when an edit transition cannot
// be persisted.
func (s *Service) applyStateTransition(ctx context.Context, runStore RunStore, transition stateTransition) (store.Run, error) {
	next := transition.Next
	if next.Revision <= transition.Previous.Revision {
		next.Revision = transition.Previous.Revision + 1
	}
	if store.IsTerminalStatus(next.Status) && !store.IsTerminalStatus(transition.Previous.Status) && next.TerminalAt.IsZero() {
		next.TerminalAt = next.UpdatedAt
		if next.TerminalAt.IsZero() {
			next.TerminalAt = s.deps.Now().UTC()
		}
	}
	if !store.IsTerminalStatus(next.Status) {
		next.TerminalAt = time.Time{}
	}
	if err := store.ValidateRun(next); err != nil {
		return next, fmt.Errorf("validate run before reserving %s effect: %w", store.PendingEffectKindStateTransition, err)
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.journal().ApplyStateTransition(ctx, runStore, transition, next)
	}
	return s.applyLegacyStateTransition(ctx, runStore, transition, next)
}

// applyLegacyStateTransition retains the pre-journal behavior for embedders
// that supply an older RunStore implementation. The production SQLite store
// implements PendingEffectStore and always uses the restart-safe path.
func (s *Service) applyLegacyStateTransition(ctx context.Context, runStore RunStore, transition stateTransition, next store.Run) (store.Run, error) {
	if transition.PersistBeforeEffects {
		next.UpdatedAt = s.deps.Now().UTC()
		if transition.CreateComment {
			return next, errors.New("cannot persist a command before creating its status comment")
		}
		if err := saveCommandRun(ctx, runStore, transition.Previous.Revision, next); err != nil {
			return next, fmt.Errorf("persist state transition before GitHub effects: %w", err)
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
				return next, fmt.Errorf("record evaluation state transition: %w", err)
			}
		}
	}
	oldLabels := append([]string(nil), transition.Issue.Labels...)
	newLabels := replaceFactoryState(oldLabels, factoryLabelForStatus(next.Status))
	if err := s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, newLabels); err != nil {
		return next, fmt.Errorf("set issue #%d state: %w", next.IssueNumber, err)
	}
	if transition.CreateComment {
		comment, err := s.deps.GitHub.CreateIssueComment(ctx, transition.Repository, next.IssueNumber, statusCommentBody(next))
		if err != nil {
			return next, fmt.Errorf("create status comment: %w", err)
		}
		if strings.TrimSpace(comment.ID) == "" {
			return next, errors.New("create status comment returned an empty comment id")
		}
		next.StatusCommentID = comment.ID
	} else {
		if err := s.deps.GitHub.EditIssueComment(ctx, transition.Repository, next.StatusCommentID, statusCommentBody(next)); err != nil {
			_ = s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, oldLabels)
			return next, fmt.Errorf("edit status comment: %w", err)
		}
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if transition.PersistBeforeEffects {
		return next, nil
	}
	if err := saveRunWithRetry(ctx, runStore, next); err != nil {
		if !transition.CreateComment {
			compensationErrors := []error{
				s.deps.GitHub.EditIssueComment(ctx, transition.Repository, transition.Previous.StatusCommentID, statusCommentBody(transition.Previous)),
				s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, oldLabels),
			}
			return next, errors.Join(append([]error{fmt.Errorf("persist state transition: %w", err)}, compensationErrors...)...)
		}
		return next, fmt.Errorf("persist claim state: %w", err)
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
			return next, fmt.Errorf("record evaluation state transition: %w", err)
		}
	}
	if transition.InvalidateAllResults {
		if err := invalidateAllRunResults(ctx, runStore, next.ID); err != nil {
			return next, err
		}
	} else if transition.InvalidateResults {
		if err := invalidateRunResults(ctx, runStore, next.ID); err != nil {
			return next, err
		}
	}
	return next, nil
}
