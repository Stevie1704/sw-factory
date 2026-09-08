package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// labelTransitionHandler owns the standalone complete issue-label replacement.
type labelTransitionHandler struct {
	now       func() time.Time
	issues    issueClient
	projector runProjector
}

// ApplyLabels reserves and applies one complete issue-label replacement.
func (j *Journal) ApplyLabels(ctx context.Context, runStore RunStore, runID string, repository github.Repository, issueNumber int, labels []string) error {
	handler := mustApplyHandler[labelTransitionHandler](j.dispatcher, store.PendingEffectKindLabelTransition)
	return handler.applyJournaled(ctx, runStore, runID, labelTransitionEffectPayload{
		Repository:  repository,
		IssueNumber: issueNumber,
		Labels:      append([]string(nil), labels...),
	})
}

// applyJournaled reserves one label replacement before crossing the GitHub
// mutation seam.
func (h labelTransitionHandler) applyJournaled(ctx context.Context, runStore RunStore, runID string, payload labelTransitionEffectPayload) error {
	identity := fmt.Sprintf("issue=%d\x00labels=%s", payload.IssueNumber, strings.Join(payload.Labels, "\x00"))
	effect, err := reserve(h.now, runID, store.PendingEffectKindLabelTransition, identity, payload)
	if err != nil {
		return err
	}
	return withPendingEffect(ctx, runStore, effect, applier(func() error {
		return h.apply(ctx, payload)
	}))
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
func (h labelTransitionHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload labelTransitionEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
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
