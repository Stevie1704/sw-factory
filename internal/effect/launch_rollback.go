package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// launchRollbackHandler owns the stop proof that must come before a failed
// launch's invocation becomes terminal.
type launchRollbackHandler struct {
	now       func() time.Time
	lifecycle lifecycle
	projector runProjector
}

// RollBackLaunch stops the worker of a failed launch and only then saves the
// terminal invocation. An empty workerID means
// that the launch did not start a worker. If the stop fails, the invocation stays active and the
// rollback stays journaled, so the migration gate refuses the store and the
// next reconciliation retries the stop.
func (j *Journal) RollBackLaunch(ctx context.Context, invocationStore InvocationStore, terminal store.Invocation, workerID string) error {
	handler := mustApplyHandler[launchRollbackHandler](j.dispatcher, store.PendingEffectKindLaunchRollback)
	return handler.rollBack(ctx, invocationStore, launchRollbackEffectPayload{Invocation: terminal, WorkerID: workerID})
}

// rollBack reserves and performs one launch rollback.
func (h launchRollbackHandler) rollBack(ctx context.Context, invocationStore InvocationStore, rollback launchRollbackEffectPayload) error {
	effect, err := reserve(h.now, rollback.Invocation.RunID, store.PendingEffectKindLaunchRollback, rollback.Invocation.ID, rollback)
	if err != nil {
		return err
	}
	return withPendingEffect(ctx, invocationStore, effect, applier(func() error {
		return h.apply(ctx, invocationStore, rollback)
	}))
}

// apply stops the worker and then saves the terminal invocation.
func (h launchRollbackHandler) apply(ctx context.Context, invocationStore InvocationStore, rollback launchRollbackEffectPayload) error {
	if rollback.WorkerID != "" {
		if h.lifecycle == nil {
			return errors.New("worker lifecycle is required to stop a rolled-back launch")
		}
		if err := h.lifecycle.StopWorker(ctx, rollback.WorkerID); err != nil {
			return fmt.Errorf("stop worker %q of rolled-back invocation %q: %w", rollback.WorkerID, rollback.Invocation.ID, err)
		}
	}
	if err := invocationStore.SaveInvocation(ctx, rollback.Invocation); err != nil {
		return fmt.Errorf("persist rolled-back invocation %q: %w", rollback.Invocation.ID, err)
	}
	return nil
}

// Replay retries the worker stop of a journaled rollback. It refuses an
// invocation that reached another terminal state after the reservation,
// because that state is a newer workflow projection.
func (h launchRollbackHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var rollback launchRollbackEffectPayload
	if err := decodePendingEffect(effect, &rollback); err != nil {
		return store.Run{}, err
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support launch rollback replay")
	}
	current, err := invocationStore.Invocation(ctx, effect.RunID, rollback.Invocation.ID)
	if err != nil {
		return store.Run{}, fmt.Errorf("read invocation during launch rollback replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("invocation %q disappeared during launch rollback replay", rollback.Invocation.ID)
	}
	if current.Status != store.InvocationStatusActive && current.Status != rollback.Invocation.Status {
		return store.Run{}, workflowProjectionFailuref("rolled-back invocation %q is %s, not active or %s", current.ID, current.Status, rollback.Invocation.Status)
	}
	if err := h.apply(ctx, invocationStore, rollback); err != nil {
		return store.Run{}, fmt.Errorf("replay launch rollback: %w", err)
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "launch rollback"); err != nil {
		return store.Run{}, err
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "launch rollback replay")
}
