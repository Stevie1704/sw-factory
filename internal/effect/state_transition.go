package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// stateTransitionHandler owns the paired issue-label and status-comment
// projection of one persisted run revision.
type stateTransitionHandler struct {
	now       func() time.Time
	labels    issueProjection
	projector runProjector
	lifecycle lifecycle
}

// ApplyStateTransition reserves the complete label and comment transition
// before crossing GitHub's mutation boundary. The reservation remains until
// both the projection and the durable run state are complete.
func (j *Journal) ApplyStateTransition(ctx context.Context, runStore RunStore, transition StateTransition, next store.Run) (store.Run, error) {
	handler := mustApplyHandler[stateTransitionHandler](j.dispatcher, store.PendingEffectKindStateTransition)
	return handler.apply(ctx, runStore, transition, next)
}

// apply reserves and performs one journaled state transition.
func (h stateTransitionHandler) apply(ctx context.Context, runStore RunStore, transition StateTransition, next store.Run) (store.Run, error) {
	payload := stateTransitionEffectPayload{
		Repository:           transition.Repository,
		Issue:                transition.Issue,
		Previous:             transition.Previous,
		Next:                 next,
		CreateComment:        transition.CreateComment,
		StopWorker:           transition.StopWorker,
		InvalidateResults:    transition.InvalidateResults,
		InvalidateAllResults: transition.InvalidateAllResults,
	}
	effect, err := reserve(h.now, next.ID, store.PendingEffectKindStateTransition, fmt.Sprintf("revision=%d", next.Revision), payload)
	if err != nil {
		return next, err
	}
	apply := func() error {
		if transition.StopWorker {
			if err := h.lifecycle.StopActiveWorkers(ctx, runStore, transition.Previous); err != nil {
				return err
			}
		}
		if transition.PersistBeforeEffects {
			next.UpdatedAt = h.now().UTC()
			if transition.CreateComment {
				return errors.New("cannot persist a command before creating its status comment")
			}
			if err := h.projector.SaveAtRevision(ctx, runStore, transition.Previous.Revision, next); err != nil {
				return fmt.Errorf("persist state transition before GitHub effects: %w", err)
			}
			if err := h.projector.RecordTransition(ctx, runStore, transition.Previous, next, next.UpdatedAt); err != nil {
				return fmt.Errorf("record evaluation state transition: %w", err)
			}
		}
		if err := h.labels.applyStateTransition(ctx, &next, transition); err != nil {
			return err
		}
		next.UpdatedAt = h.now().UTC()
		if !transition.PersistBeforeEffects {
			scope := scopeFor(transition.InvalidateAllResults, transition.InvalidateResults)
			if err := h.persist(ctx, runStore, scope, transition.Previous.Revision, next, "state transition"); err != nil {
				return err
			}
			if err := h.projector.RecordTransition(ctx, runStore, transition.Previous, next, next.UpdatedAt); err != nil {
				return fmt.Errorf("record evaluation state transition: %w", err)
			}
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(apply)); err != nil {
		return next, err
	}
	return next, nil
}

// persist writes the transition's run revision, invalidating the superseded
// packet results when the transition crosses a packet revision boundary. A
// store without an atomic seam persists and invalidates in two steps.
//
// description names the operation in a persistence failure, so an apply and a
// replay of the same transition keep their own durable error text.
func (h stateTransitionHandler) persist(ctx context.Context, runStore RunStore, scope invalidationScope, expected int64, next store.Run, description string) error {
	switch scope {
	case invalidateAllResults:
		atomic, err := h.projector.SaveInvalidatingAllResults(ctx, runStore, expected, next)
		if err != nil {
			return fmt.Errorf("persist %s and invalidate all results: %w", description, err)
		}
		if atomic {
			return nil
		}
		if err := h.projector.SaveAtRevision(ctx, runStore, expected, next); err != nil {
			return fmt.Errorf("persist %s: %w", description, err)
		}
		return h.projector.InvalidateAllResults(ctx, runStore, next.ID)
	case invalidateResults:
		atomic, err := h.projector.SaveInvalidatingResults(ctx, runStore, expected, next)
		if err != nil {
			return fmt.Errorf("persist %s and invalidate results: %w", description, err)
		}
		if atomic {
			return nil
		}
		if err := h.projector.SaveAtRevision(ctx, runStore, expected, next); err != nil {
			return fmt.Errorf("persist %s: %w", description, err)
		}
		return h.projector.InvalidateResults(ctx, runStore, next.ID)
	default:
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist %s: %w", description, err)
		}
	}
	return nil
}

// invalidationScope selects how much of a superseded packet a transition
// discards along with its run revision.
type invalidationScope int

const (
	// retainResults keeps every prior gate and invocation projection.
	retainResults invalidationScope = iota
	// invalidateResults discards the superseded invocation and gate results.
	invalidateResults
	// invalidateAllResults additionally discards the superseded baseline.
	invalidateAllResults
)

// scopeFor selects the invalidation a transition reserved.
func scopeFor(invalidateAll, invalidate bool) invalidationScope {
	switch {
	case invalidateAll:
		return invalidateAllResults
	case invalidate:
		return invalidateResults
	default:
		return retainResults
	}
}

// Replay restores the complete persisted run revision associated with a
// label/comment effect and then acknowledges its journal.
func (h stateTransitionHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload stateTransitionEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if payload.Next.ID == "" || payload.Next.ID != effect.RunID {
		return store.Run{}, errors.New("pending state transition run identity does not match its journal")
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindStateTransition, payload.Next); err != nil {
		return store.Run{}, err
	}
	current, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run while replaying state transition: %w", err)
	}
	next := payload.Next
	if current != nil {
		if current.ID != next.ID {
			return store.Run{}, workflowProjectionFailuref("pending state transition belongs to run %q, current run is %q", next.ID, current.ID)
		}
		if current.Revision > next.Revision {
			return store.Run{}, workflowProjectionFailuref("pending state transition revision %d is older than current revision %d", next.Revision, current.Revision)
		}
		if current.Revision == next.Revision {
			// The run may already be persisted while the final comment identity was
			// still in memory. Preserve any newer durable fields, then fill the
			// effect-owned identity from the replay payload.
			if current.StatusCommentID != "" {
				next.StatusCommentID = current.StatusCommentID
			}
		}
		if current.Revision < next.Revision && current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("pending state transition expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
	}
	if payload.StopWorker {
		if err := h.lifecycle.StopActiveWorkers(ctx, runStore, payload.Previous); err != nil {
			return store.Run{}, fmt.Errorf("stop worker during state-transition replay: %w", err)
		}
	}
	transition := StateTransition{
		Repository:           payload.Repository,
		Issue:                payload.Issue,
		Previous:             payload.Previous,
		Next:                 next,
		CreateComment:        payload.CreateComment,
		StopWorker:           payload.StopWorker,
		InvalidateResults:    payload.InvalidateResults,
		InvalidateAllResults: payload.InvalidateAllResults,
	}
	if err := h.labels.applyStateTransition(ctx, &next, transition); err != nil {
		return store.Run{}, fmt.Errorf("replay state transition effects: %w", err)
	}
	next.UpdatedAt = h.now().UTC()
	if current == nil || current.Revision < next.Revision {
		scope := scopeFor(payload.InvalidateAllResults, payload.InvalidateResults)
		if err := h.persist(ctx, runStore, scope, payload.Previous.Revision, next, "replayed state transition"); err != nil {
			return store.Run{}, err
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "state transition"); err != nil {
		return store.Run{}, err
	}
	return next, nil
}
