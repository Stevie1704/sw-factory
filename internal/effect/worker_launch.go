package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// workerLaunchHandler owns the worker creation or reuse reserved before the
// coordinator crosses into Docker.
type workerLaunchHandler struct {
	now       func() time.Time
	worker    workerLauncher
	projector runProjector
}

// StartWorker reserves a worker launch before crossing into Docker.
// DockerRuntime makes Start itself idempotent for an exact run/image/mount
// identity, so replaying a completed launch is safe.
func (j *Journal) StartWorker(ctx context.Context, runStore RunStore, request worker.StartRequest) error {
	handler := mustApplyHandler[workerLaunchHandler](j.dispatcher, store.PendingEffectKindWorkerLaunch)
	return handler.start(ctx, runStore, request)
}

// start reserves and performs one worker launch.
func (h workerLaunchHandler) start(ctx context.Context, runStore RunStore, request worker.StartRequest) error {
	payload := workerLaunchEffectPayload{Request: request}
	effect, err := reserve(h.now, request.RunID, store.PendingEffectKindWorkerLaunch, request.Role+"\x00"+request.InvocationPath+"\x00"+request.ResultPath, payload)
	if err != nil {
		return err
	}
	return withPendingEffect(ctx, runStore, effect, applier(func() error {
		if h.worker == nil {
			return errors.New("worker runtime is required")
		}
		return h.worker.Start(ctx, request)
	}))
}

// Replay completes a worker reservation from its portable start request and
// leaves the durable run projection unchanged.
func (h workerLaunchHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload workerLaunchEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if h.worker == nil {
		return store.Run{}, errors.New("worker runtime is required to replay worker launch")
	}
	if err := h.worker.Start(ctx, payload.Request); err != nil {
		return store.Run{}, fmt.Errorf("replay worker launch: %w", err)
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "worker launch"); err != nil {
		return store.Run{}, err
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "worker launch replay")
}
