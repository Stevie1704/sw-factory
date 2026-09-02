package factory

import (
	"context"
	"errors"

	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// newPendingEffectDispatcher registers the factory-owned replay policies at
// the protocol kernel's kind-based seam.
func newPendingEffectDispatcher(service *Service) *effectkernel.Dispatcher {
	dispatcher := effectkernel.NewDispatcher()
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindStateTransition, stateTransitionReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindLabelTransition, labelTransitionReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindWorkerLaunch, workerLaunchReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindCheckpoint, checkpointReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindPush, pushReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindPullRequest, pullRequestReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindCommitStatus, commitStatusReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindStatusComment, statusCommentReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindClarificationComment, clarificationCommentReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindResultAcceptance, resultAcceptanceReplayHandler{service: service})
	registerPendingEffectHandler(dispatcher, store.PendingEffectKindHarnessResume, harnessResumeReplayHandler{service: service})
	return dispatcher
}

// registerPendingEffectHandler makes registration failures programmer errors:
// these are factory-owned constants and should never collide or be empty.
func registerPendingEffectHandler(dispatcher *effectkernel.Dispatcher, kind store.PendingEffectKind, handler effectkernel.ReplayHandler) {
	if err := dispatcher.Register(kind, handler); err != nil {
		panic(err)
	}
}

// factoryRunStore adapts the kernel's opaque store value to the coordinator's
// richer run-store interface without exposing factory policy to the kernel.
func factoryRunStore(request effectkernel.ReplayRequest) (RunStore, error) {
	runStore, ok := request.Store.(RunStore)
	if !ok {
		return nil, errors.New("pending effect replay requires a factory run store")
	}
	return runStore, nil
}

type stateTransitionReplayHandler struct{ service *Service }

// Replay applies a state-transition effect using the factory policy.
func (h stateTransitionReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingStateTransition(ctx, runStore, request.Effect)
}

type labelTransitionReplayHandler struct{ service *Service }

// Replay applies a standalone label-transition effect using the factory policy.
func (h labelTransitionReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingLabelTransition(ctx, runStore, request.Effect)
}

type workerLaunchReplayHandler struct{ service *Service }

// Replay applies a worker-launch effect using the factory policy.
func (h workerLaunchReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingWorkerLaunch(ctx, runStore, request.Effect)
}

type checkpointReplayHandler struct{ service *Service }

// Replay applies a checkpoint effect using the factory policy.
func (h checkpointReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingCheckpoint(ctx, runStore, request.Effect)
}

type pushReplayHandler struct{ service *Service }

// Replay applies a push effect using the factory policy.
func (h pushReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingPush(ctx, runStore, request.Effect)
}

type pullRequestReplayHandler struct{ service *Service }

// Replay applies a pull-request effect using the factory policy.
func (h pullRequestReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingPullRequest(ctx, runStore, request.Effect)
}

type commitStatusReplayHandler struct{ service *Service }

// Replay applies a commit-status effect using the factory policy.
func (h commitStatusReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingCommitStatus(ctx, runStore, request.Effect)
}

type statusCommentReplayHandler struct{ service *Service }

// Replay applies a status-comment effect using the factory policy.
func (h statusCommentReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingStatusComment(ctx, runStore, request.Effect)
}

type clarificationCommentReplayHandler struct{ service *Service }

// Replay applies a clarification-comment effect using the factory policy.
func (h clarificationCommentReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingClarificationComment(ctx, runStore, request.Effect)
}

type resultAcceptanceReplayHandler struct{ service *Service }

// Replay applies a result-acceptance effect using the factory policy.
func (h resultAcceptanceReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingResultAcceptance(ctx, runStore, request.Effect)
}

type harnessResumeReplayHandler struct{ service *Service }

// Replay applies a harness-resume effect using the factory policy.
func (h harnessResumeReplayHandler) Replay(ctx context.Context, request effectkernel.ReplayRequest) (store.Run, error) {
	runStore, err := factoryRunStore(request)
	if err != nil {
		return store.Run{}, err
	}
	return h.service.replayPendingHarnessResume(ctx, runStore, request.Effect)
}
