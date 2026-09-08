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

// checkpointHandler owns the checkpoint commit and the run projection that
// immediately records it.
type checkpointHandler struct {
	now       func() time.Time
	workspace gitadapter.GitWorkspace
	labels    issueProjection
	projector runProjector
}

// Checkpoint makes a checkpoint commit and the immediate run projection one
// restart-safe operation. The marker in the Git adapter handles a commit that
// was created just before the process stopped.
func (j *Journal) Checkpoint(ctx context.Context, runStore RunStore, request gitadapter.CheckpointRequest, repository github.Repository, issue github.Issue, previous, nextTemplate store.Run) (gitadapter.CheckpointResult, store.Run, error) {
	handler := mustApplyHandler[checkpointHandler](j.dispatcher, store.PendingEffectKindCheckpoint)
	return handler.commit(ctx, runStore, request, repository, issue, previous, nextTemplate)
}

// commit reserves the checkpoint, creates it, and persists its projection.
func (h checkpointHandler) commit(ctx context.Context, runStore RunStore, request gitadapter.CheckpointRequest, repository github.Repository, issue github.Issue, previous, nextTemplate store.Run) (gitadapter.CheckpointResult, store.Run, error) {
	if err := validateRunBeforeEffect(store.PendingEffectKindCheckpoint, nextTemplate); err != nil {
		return gitadapter.CheckpointResult{}, nextTemplate, err
	}
	payload := checkpointEffectPayload{
		Request: checkpointRequestJSON{
			RunID:        request.RunID,
			WorktreePath: request.WorktreePath,
			ParentSHA:    request.ParentSHA,
			Kind:         string(request.Kind),
			Paths:        append([]string(nil), request.Paths...),
			Message:      request.Message,
		},
		Repository: repository,
		Issue:      issue,
		Previous:   previous,
		Next:       nextTemplate,
	}
	effect, err := reserve(h.now, request.RunID, store.PendingEffectKindCheckpoint, request.ParentSHA+"\x00"+string(request.Kind)+"\x00"+strings.Join(request.Paths, "\x00"), payload)
	if err != nil {
		return gitadapter.CheckpointResult{}, nextTemplate, err
	}
	var checkpoint gitadapter.CheckpointResult
	next := nextTemplate
	action := func() error {
		checkpoint, err = h.workspace.CreateCheckpoint(ctx, request)
		if err != nil {
			return fmt.Errorf("create checkpoint: %w", err)
		}
		if !github.ValidCommitSHA(checkpoint.SHA) {
			return errors.New("GitWorkspace returned an invalid checkpoint SHA")
		}
		next.AcceptedImplementationCheckpointSHA = ""
		next.CheckpointSHA = checkpoint.SHA
		if request.Kind == gitadapter.CheckpointKindTest {
			next.TestCheckpointSHA = checkpoint.SHA
		}
		if err := validateRunBeforeEffect(store.PendingEffectKindCheckpoint, next); err != nil {
			return err
		}
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
			return fmt.Errorf("persist checkpoint run projection: %w", err)
		}
		if err := h.projector.RecordTransition(ctx, runStore, previous, next, next.UpdatedAt); err != nil {
			return fmt.Errorf("record checkpoint state transition: %w", err)
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(action)); err != nil {
		return checkpoint, next, err
	}
	return checkpoint, next, nil
}

// Replay completes a checkpoint reservation by replaying the idempotent Git
// marker and its persisted run transition.
func (h checkpointHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload checkpointEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindCheckpoint, payload.Next); err != nil {
		return store.Run{}, err
	}
	if h.workspace == nil {
		return store.Run{}, errors.New("GitWorkspace is required to replay checkpoint")
	}
	current, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during checkpoint replay: %w", err)
	}
	if current != nil {
		if current.ID != effect.RunID {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay belongs to run %q, current run is %q", effect.RunID, current.ID)
		}
		if current.Revision > payload.Next.Revision {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay revision %d is older than current revision %d", payload.Next.Revision, current.Revision)
		}
		if current.Revision < payload.Next.Revision && current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
	}
	checkpointInput := checkpointRequest(payload.Request)
	checkpoint, err := h.workspace.CreateCheckpoint(ctx, checkpointInput)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay checkpoint: %w", err)
	}
	if !github.ValidCommitSHA(checkpoint.SHA) {
		return store.Run{}, errors.New("replayed checkpoint returned an invalid SHA")
	}
	next := payload.Next
	next.AcceptedImplementationCheckpointSHA = ""
	next.CheckpointSHA = checkpoint.SHA
	if checkpointInput.Kind == gitadapter.CheckpointKindTest {
		next.TestCheckpointSHA = checkpoint.SHA
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindCheckpoint, next); err != nil {
		return store.Run{}, err
	}
	if err := h.labels.applyStateTransition(ctx, &next, StateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay checkpoint state projection: %w", err)
	}
	next.UpdatedAt = h.now().UTC()
	if current == nil || current.Revision < next.Revision {
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed checkpoint: %w", err)
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "checkpoint"); err != nil {
		return store.Run{}, err
	}
	return next, nil
}
