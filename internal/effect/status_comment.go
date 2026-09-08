package effect

import (
	"context"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// statusCommentHandler owns a command's local watermark together with the
// status-comment edit that publishes it.
type statusCommentHandler struct {
	now       func() time.Time
	labels    issueProjection
	projector RunProjector
}

// PersistCommandProjection journals a command's local watermark together with
// its status-comment mutation. The local projection is part of the action so a
// process stop between the two writes leaves a replayable intent instead of an
// apparently processed but stale command.
func (j *Journal) PersistCommandProjection(ctx context.Context, runStore RunStore, repository github.Repository, previous, next store.Run) (store.Run, error) {
	return j.statusComment.persist(ctx, runStore, repository, previous, next)
}

// persist reserves the watermark and its status-comment edit as one effect.
func (h statusCommentHandler) persist(ctx context.Context, runStore RunStore, repository github.Repository, previous, next store.Run) (store.Run, error) {
	if err := ValidateRunBeforeEffect(store.PendingEffectKindStatusComment, next); err != nil {
		return next, err
	}
	payload := statusCommentEffectPayload{Repository: repository, Previous: previous, Next: next}
	effect, err := reserve(h.now, next.ID, store.PendingEffectKindStatusComment, fmt.Sprintf("revision=%d\x00comment=%s", next.Revision, next.ProcessedCommentID), payload)
	if err != nil {
		return next, err
	}
	action := func() error {
		current, err := h.projector.Read(ctx, runStore)
		if err != nil {
			return fmt.Errorf("read run before command projection: %w", err)
		}
		if current == nil {
			return fmt.Errorf("run %q disappeared before command projection", next.ID)
		}
		switch {
		case current.Revision < next.Revision:
			if current.Revision != previous.Revision {
				return fmt.Errorf("command projection expected revision %d, found %d", previous.Revision, current.Revision)
			}
			if err := h.projector.SaveAtRevision(ctx, runStore, previous.Revision, next); err != nil {
				return fmt.Errorf("persist command watermark: %w", err)
			}
		case current.Revision == next.Revision:
			if current.ProcessedCommentID != next.ProcessedCommentID || current.LastCommandName != next.LastCommandName {
				return fmt.Errorf("command projection revision %d belongs to another command", current.Revision)
			}
		default:
			if current.ProcessedCommentID != next.ProcessedCommentID {
				return fmt.Errorf("command projection was superseded at revision %d", current.Revision)
			}
		}
		return h.labels.applyStatusComment(ctx, payload)
	}
	if err := WithPendingEffect(ctx, runStore, effect, applier(action)); err != nil {
		return next, err
	}
	return next, nil
}

// Replay finishes a command projection after a process boundary. It persists
// the watermark only when it is still missing, then recognizes or applies the
// exact status-comment body before clearing intent.
func (h statusCommentHandler) Replay(ctx context.Context, request ReplayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload statusCommentEffectPayload
	if err := DecodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindStatusComment, payload.Next); err != nil {
		return store.Run{}, err
	}
	current, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during status-comment replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during status-comment replay", effect.RunID)
	}
	next := payload.Next
	if current.Revision < next.Revision {
		if current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("status-comment replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
		if err := h.projector.SaveAtRevision(ctx, runStore, payload.Previous.Revision, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed command watermark: %w", err)
		}
	} else if current.Revision == next.Revision {
		if current.ProcessedCommentID != next.ProcessedCommentID || current.LastCommandName != next.LastCommandName {
			return store.Run{}, workflowProjectionFailuref("status-comment replay revision %d belongs to another command", current.Revision)
		}
	} else if current.ProcessedCommentID != next.ProcessedCommentID {
		return store.Run{}, workflowProjectionFailuref("status-comment replay was superseded at revision %d", current.Revision)
	}
	if err := h.labels.applyStatusComment(ctx, payload); err != nil {
		return store.Run{}, fmt.Errorf("replay status comment: %w", err)
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "status comment"); err != nil {
		return store.Run{}, err
	}
	if current.Revision < next.Revision {
		return next, nil
	}
	return *current, nil
}
