package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// pushHandler owns the run-branch publication reserved before the remote is
// mutated.
type pushHandler struct {
	now       func() time.Time
	workspace gitadapter.GitWorkspace
	projector runProjector
}

// Push publishes one run branch while recognizing an already matching remote
// head. A repeated Git push is transport-safe, but the remote head read makes
// the semantic effect exactly-once at the coordinator seam.
func (j *Journal) Push(ctx context.Context, runStore RunStore, runID string, request gitadapter.PushRequest, expectedSHA string) error {
	handler := mustApplyHandler[pushHandler](j.dispatcher, store.PendingEffectKindPush)
	return handler.publish(ctx, runStore, runID, request, expectedSHA)
}

// publish reserves and performs one branch push.
func (h pushHandler) publish(ctx context.Context, runStore RunStore, runID string, request gitadapter.PushRequest, expectedSHA string) error {
	payload := pushEffectPayload{
		Request:     pushRequestJSON{WorktreePath: request.WorktreePath, Branch: request.Branch},
		ExpectedSHA: expectedSHA,
	}
	effect, err := reserve(h.now, runID, store.PendingEffectKindPush, request.WorktreePath+"\x00"+request.Branch+"\x00"+expectedSHA, payload)
	if err != nil {
		return err
	}
	return withPendingEffect(ctx, runStore, effect, applier(func() error {
		return pushOnce(ctx, h.workspace, request, expectedSHA)
	}))
}

// pushOnce observes the remote branch when the adapter supports it and only
// invokes the mutating push when the expected checkpoint is not already there.
func pushOnce(ctx context.Context, workspace gitadapter.GitWorkspace, request gitadapter.PushRequest, expectedSHA string) error {
	if workspace == nil {
		return errors.New("GitWorkspace is required for push")
	}
	if inspector, ok := workspace.(gitadapter.RemoteBranchInspector); ok {
		head, err := inspector.RemoteBranchHead(ctx, request)
		if err != nil {
			return fmt.Errorf("inspect remote branch before push: %w", err)
		}
		if strings.TrimSpace(expectedSHA) != "" && head == expectedSHA {
			return nil
		}
	}
	state, err := workspace.Inspect(ctx, request.WorktreePath)
	if err != nil {
		return fmt.Errorf("inspect local worktree before push: %w", err)
	}
	if strings.TrimSpace(expectedSHA) != "" && state.HeadSHA != expectedSHA {
		return fmt.Errorf("local worktree HEAD %q does not match pending push checkpoint %q", state.HeadSHA, expectedSHA)
	}
	if err := workspace.Push(ctx, request); err != nil {
		return err
	}
	return nil
}

// Replay completes or recognizes a branch push recorded before a process
// interruption.
func (h pushHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload pushEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if h.workspace == nil {
		return store.Run{}, errors.New("GitWorkspace is required to replay push")
	}
	pushRequest := gitadapter.PushRequest{WorktreePath: payload.Request.WorktreePath, Branch: payload.Request.Branch}
	if err := pushOnce(ctx, h.workspace, pushRequest, payload.ExpectedSHA); err != nil {
		return store.Run{}, fmt.Errorf("replay push: %w", err)
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "push"); err != nil {
		return store.Run{}, err
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "push replay")
}
