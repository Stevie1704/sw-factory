package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// commitStatusHandler owns one exact-SHA commit status and the branch
// publication that makes the remote able to resolve its checkpoint.
type commitStatusHandler struct {
	now       func() time.Time
	statuses  github.CommitStatusPublisher
	workspace gitadapter.GitWorkspace
	projector RunProjector
}

// CommitStatusPublisher wraps a commit-status seam with durable idempotency,
// so an ambiguous response cannot create a second semantic status.
func (j *Journal) CommitStatusPublisher(runStore RunStore, runID string, delegate github.CommitStatusPublisher) github.CommitStatusPublisher {
	return journaledCommitStatus{handler: j.commitStatus, runStore: runStore, runID: runID, delegate: delegate}
}

// journaledCommitStatus reserves each status before publishing it.
type journaledCommitStatus struct {
	handler  commitStatusHandler
	runStore RunStore
	runID    string
	delegate github.CommitStatusPublisher
}

// CreateCommitStatus publishes or recognizes one exact-SHA status without
// creating a second semantic status after a process interruption.
func (p journaledCommitStatus) CreateCommitStatus(ctx context.Context, repository github.Repository, status github.CommitStatus) error {
	payload := commitStatusEffectPayload{Repository: repository, Status: status}
	effect, err := reserve(p.handler.now, p.runID, store.PendingEffectKindCommitStatus, status.SHA+"\x00"+status.Context+"\x00"+string(status.State)+"\x00"+status.Description, payload)
	if err != nil {
		return err
	}
	return WithPendingEffect(ctx, p.runStore, effect, applier(func() error {
		if reader, ok := p.delegate.(github.CommitStatusReader); ok {
			statuses, err := reader.ListCommitStatuses(ctx, repository, status.SHA)
			if err != nil {
				return fmt.Errorf("inspect existing commit status: %w", err)
			}
			for _, existing := range statuses {
				if sameCommitStatus(existing, status) {
					return nil
				}
			}
		}
		return p.delegate.CreateCommitStatus(ctx, repository, status)
	}))
}

// Replay recognizes or republishes one exact-SHA status and then clears its
// durable reservation. A reader is required during recovery so an adapter
// cannot blindly create a duplicate after an ambiguous response.
func (h commitStatusHandler) Replay(ctx context.Context, request ReplayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload commitStatusEffectPayload
	if err := DecodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if h.statuses == nil {
		return store.Run{}, errors.New("commit-status publisher is required to replay status")
	}
	reader, ok := h.statuses.(github.CommitStatusReader)
	if !ok {
		return store.Run{}, errors.New("commit-status reader is required to replay status safely")
	}
	run, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during commit status replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during commit status replay", effect.RunID)
	}
	if err := h.publishStatusCheckpoint(ctx, *run, payload.Status.SHA); err != nil {
		return store.Run{}, err
	}
	statuses, err := reader.ListCommitStatuses(ctx, payload.Repository, payload.Status.SHA)
	if err != nil {
		return store.Run{}, fmt.Errorf("inspect commit status during replay: %w", err)
	}
	found := false
	for _, status := range statuses {
		if sameCommitStatus(status, payload.Status) {
			found = true
			break
		}
	}
	if !found {
		if err := h.statuses.CreateCommitStatus(ctx, payload.Repository, payload.Status); err != nil {
			return store.Run{}, fmt.Errorf("replay commit status: %w", err)
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "commit status"); err != nil {
		return store.Run{}, err
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "commit status replay")
}

// publishStatusCheckpoint makes the remote resolve the commit a journaled
// status names before the status is inspected or republished. An interrupted
// attempt can journal a status for a checkpoint that never reached GitHub, and
// GitHub answers every later attempt with an unknown-SHA rejection, so the
// reservation could otherwise never be completed. Only the run's own
// checkpoint is published this way, and the push observes the remote head
// first, so it adds no unobserved second mutation.
func (h commitStatusHandler) publishStatusCheckpoint(ctx context.Context, run store.Run, sha string) error {
	if h.workspace == nil || sha == "" || sha != run.CheckpointSHA || strings.TrimSpace(run.Branch) == "" {
		return nil
	}
	request := gitadapter.PushRequest{WorktreePath: run.Worktree, Branch: run.Branch}
	if err := pushOnce(ctx, h.workspace, request, sha); err != nil {
		return fmt.Errorf("publish checkpoint %s before commit status replay: %w", sha, err)
	}
	return nil
}
