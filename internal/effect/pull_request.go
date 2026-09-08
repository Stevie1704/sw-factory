package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// pullRequestHandler owns draft pull-request mutation and the run projection
// that records the resulting pull-request identity.
type pullRequestHandler struct {
	now       func() time.Time
	clients   github.PullRequestClient
	labels    issueProjection
	projector RunProjector
}

// UpsertPullRequest performs a branch-scoped, idempotent pull-request
// mutation. The expected number prevents a replay from silently attaching to
// another pull request. It is exported because the pre-journal compatibility
// path performs the same idempotent mutation without a reservation.
func UpsertPullRequest(ctx context.Context, client github.PullRequestClient, repository github.Repository, expectedNumber int, request github.PullRequestRequest) (github.PullRequest, error) {
	if client == nil {
		return github.PullRequest{}, errors.New("pull-request client is required")
	}
	existing, err := client.FindPullRequest(ctx, repository, request.HeadBranch, request.BaseBranch)
	if err != nil {
		return github.PullRequest{}, err
	}
	if existing.Number > 0 {
		if expectedNumber > 0 && existing.Number != expectedNumber {
			return github.PullRequest{}, fmt.Errorf("pull request changed from #%d to #%d during replay", expectedNumber, existing.Number)
		}
		if samePullRequestRequest(existing, request) {
			return existing, nil
		}
		updated, err := client.UpdatePullRequest(ctx, repository, existing.Number, request)
		if err != nil {
			return github.PullRequest{}, err
		}
		if updated.Number <= 0 {
			return github.PullRequest{}, errors.New("pull-request update returned no pull-request identity")
		}
		return updated, nil
	}
	if expectedNumber > 0 {
		return github.PullRequest{}, fmt.Errorf("pull request #%d disappeared before replay", expectedNumber)
	}
	created, err := client.CreatePullRequest(ctx, repository, request)
	if err != nil {
		return github.PullRequest{}, err
	}
	if created.Number <= 0 {
		return github.PullRequest{}, errors.New("pull-request creation returned no pull-request identity")
	}
	return created, nil
}

// UpsertPullRequestAndPersist journals a draft pull-request mutation together
// with the run projection that records its identity. Recovery can therefore
// discover a created pull request and finish the missing durable state without
// creating a second one.
func (j *Journal) UpsertPullRequestAndPersist(ctx context.Context, runStore RunStore, repository github.Repository, issue github.Issue, previous, next store.Run, request github.PullRequestRequest, expectedNumber int) (github.PullRequest, store.Run, error) {
	return j.pullRequest.upsertAndPersist(ctx, runStore, repository, issue, previous, next, request, expectedNumber)
}

// UpdatePullRequest journals a standalone generated-body update, such as
// review regeneration, when no run-state change accompanies it.
func (j *Journal) UpdatePullRequest(ctx context.Context, runStore RunStore, runID string, repository github.Repository, number int, request github.PullRequestRequest) error {
	return j.pullRequest.update(ctx, runStore, runID, repository, number, request)
}

// upsertAndPersist reserves the pull-request mutation and its run projection.
func (h pullRequestHandler) upsertAndPersist(ctx context.Context, runStore RunStore, repository github.Repository, issue github.Issue, previous, next store.Run, request github.PullRequestRequest, expectedNumber int) (github.PullRequest, store.Run, error) {
	if err := ValidateRunBeforeEffect(store.PendingEffectKindPullRequest, next); err != nil {
		return github.PullRequest{}, next, err
	}
	payload := pullRequestEffectPayload{Repository: repository, Number: expectedNumber, Request: request, PersistRun: true, Issue: issue, Previous: previous, Next: next}
	effect, err := reserve(h.now, next.ID, store.PendingEffectKindPullRequest, request.HeadBranch+"\x00"+request.BaseBranch+"\x00"+request.Body, payload)
	if err != nil {
		return github.PullRequest{}, next, err
	}
	var pullRequest github.PullRequest
	apply := func() error {
		pullRequest, err = UpsertPullRequest(ctx, h.clients, repository, expectedNumber, request)
		if err != nil {
			return fmt.Errorf("upsert draft pull request: %w", err)
		}
		next.PullRequestNumber = pullRequest.Number
		next.PullRequestURL = pullRequest.URL
		if err := h.labels.applyStateTransition(ctx, &next, StateTransition{
			Repository: repository,
			Issue:      issue,
			Previous:   previous,
			Next:       next,
		}); err != nil {
			return err
		}
		next.UpdatedAt = h.now().UTC()
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist draft pull-request projection: %w", err)
		}
		if err := h.projector.RecordTransition(ctx, runStore, previous, next, next.UpdatedAt); err != nil {
			return fmt.Errorf("record draft pull-request transition: %w", err)
		}
		return nil
	}
	if err := WithPendingEffect(ctx, runStore, effect, applier(apply)); err != nil {
		return pullRequest, next, err
	}
	return pullRequest, next, nil
}

// update reserves a standalone generated-body update.
func (h pullRequestHandler) update(ctx context.Context, runStore RunStore, runID string, repository github.Repository, number int, request github.PullRequestRequest) error {
	payload := pullRequestEffectPayload{Repository: repository, Number: number, Request: request}
	effect, err := reserve(h.now, runID, store.PendingEffectKindPullRequest, fmt.Sprintf("update=%d\x00%s", number, request.Body), payload)
	if err != nil {
		return err
	}
	return WithPendingEffect(ctx, runStore, effect, applier(func() error {
		_, err := UpsertPullRequest(ctx, h.clients, repository, number, request)
		return err
	}))
}

// Replay finishes a pull-request mutation and its run projection from the
// persisted request, recognizing an already-created pull request by branch
// identity.
func (h pullRequestHandler) Replay(ctx context.Context, request ReplayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload pullRequestEffectPayload
	if err := DecodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if payload.PersistRun {
		if err := validateRunBeforeReplay(store.PendingEffectKindPullRequest, payload.Next); err != nil {
			return store.Run{}, err
		}
	}
	if h.clients == nil {
		return store.Run{}, errors.New("pull-request client is required to replay draft pull request")
	}
	var current *store.Run
	if payload.PersistRun {
		var currentErr error
		current, currentErr = h.projector.Read(ctx, runStore)
		if currentErr != nil {
			return store.Run{}, fmt.Errorf("read run during pull-request replay: %w", currentErr)
		}
		if current != nil {
			if current.ID != effect.RunID {
				return store.Run{}, workflowProjectionFailuref("pull-request replay belongs to run %q, current run is %q", effect.RunID, current.ID)
			}
			if current.Revision > payload.Next.Revision {
				return store.Run{}, workflowProjectionFailuref("pull-request replay revision %d is older than current revision %d", payload.Next.Revision, current.Revision)
			}
			if current.Revision < payload.Next.Revision && current.Revision != payload.Previous.Revision {
				return store.Run{}, workflowProjectionFailuref("pull-request replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
			}
		}
	}
	pullRequest, err := UpsertPullRequest(ctx, h.clients, payload.Repository, payload.Number, payload.Request)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay draft pull request: %w", err)
	}
	if !payload.PersistRun {
		if err := clearReplayedEffect(ctx, runStore, effect, "draft pull-request update"); err != nil {
			return store.Run{}, err
		}
		return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "draft pull-request update replay")
	}
	next := payload.Next
	next.PullRequestNumber = pullRequest.Number
	next.PullRequestURL = pullRequest.URL
	if err := h.labels.applyStateTransition(ctx, &next, StateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay draft pull-request state projection: %w", err)
	}
	next.UpdatedAt = h.now().UTC()
	if current == nil || current.Revision < next.Revision {
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed draft pull request: %w", err)
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "draft pull request"); err != nil {
		return store.Run{}, err
	}
	return next, nil
}
