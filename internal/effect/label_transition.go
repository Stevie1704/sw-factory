package effect

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// labelTransitionHandler owns the standalone complete issue-label replacement.
// The kind is replay-only: no current apply path reserves it, but a journal
// entry written by an earlier build must still replay.
type labelTransitionHandler struct {
	issues    IssueClient
	projector RunProjector
}

// apply observes the complete factory-owned label set before replacing it,
// making a successful mutation reported as an error safe to recognize on
// replay.
func (h labelTransitionHandler) apply(ctx context.Context, payload labelTransitionEffectPayload) error {
	if h.issues == nil {
		return errors.New("GitHub client is required for label effect")
	}
	issue, err := h.issues.Issue(ctx, payload.Repository, payload.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue for label replay: %w", err)
	}
	if sameStringSlice(issue.Labels, payload.Labels) {
		return nil
	}
	if err := h.issues.ReplaceIssueLabels(ctx, payload.Repository, payload.IssueNumber, append([]string(nil), payload.Labels...)); err != nil {
		return fmt.Errorf("replace labels for replay: %w", err)
	}
	return nil
}

// Replay completes a standalone label reservation and leaves workflow state
// untouched.
func (h labelTransitionHandler) Replay(ctx context.Context, request ReplayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload labelTransitionEffectPayload
	if err := DecodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if err := h.apply(ctx, payload); err != nil {
		return store.Run{}, err
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support label replay")
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return store.Run{}, fmt.Errorf("clear replayed labels: %w", err)
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "label replay")
}
