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

// clarificationHandler owns the coordinator-authored clarification comment and
// the run identity projection that records it.
type clarificationHandler struct {
	now          func() time.Time
	issues       issueClient
	presentation runPresentation
	projector    runProjector
}

// PublishClarificationComment reserves one clarification publication before
// the comment reaches GitHub, then records the comment identity on the run.
func (j *Journal) PublishClarificationComment(ctx context.Context, runStore RunStore, repository github.Repository, target int, run store.Run, packetVersion int, body string) (store.Run, error) {
	handler := mustApplyHandler[clarificationHandler](j.dispatcher, store.PendingEffectKindClarificationComment)
	return handler.publish(ctx, runStore, repository, target, run, packetVersion, body)
}

// publish reserves and performs one clarification publication.
func (h clarificationHandler) publish(ctx context.Context, runStore RunStore, repository github.Repository, target int, run store.Run, packetVersion int, body string) (store.Run, error) {
	if err := validateRunBeforeEffect(store.PendingEffectKindClarificationComment, run); err != nil {
		return run, err
	}
	payload := clarificationCommentEffectPayload{
		Repository: repository, Target: target, Body: body,
		PacketVersion: packetVersion,
	}
	effect, err := reserve(h.now, run.ID, store.PendingEffectKindClarificationComment, fmt.Sprintf("target=%d\x00version=%d", target, packetVersion), payload)
	if err != nil {
		return run, err
	}
	updated := run
	action := func() error {
		comment, findErr := h.findOrCreateComment(ctx, payload.Repository, target, run.ID, packetVersion, body)
		if findErr != nil {
			return findErr
		}
		updated.ClarificationCommentID = comment.ID
		updated.UpdatedAt = h.now().UTC()
		if err := h.projector.Save(ctx, runStore, updated); err != nil {
			return fmt.Errorf("persist clarification comment identity: %w", err)
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(action)); err != nil {
		return updated, err
	}
	return updated, nil
}

// findOrCreateComment observes the coordinator-owned marker and repairs its
// body before creating a question comment, making publication safe across
// response loss and stale question edits.
func (h clarificationHandler) findOrCreateComment(ctx context.Context, repository github.Repository, target int, runID string, packetVersion int, body string) (github.Comment, error) {
	if h.issues == nil {
		return github.Comment{}, errors.New("GitHub client is required for clarification publication")
	}
	comment, err := h.issues.FindStatusComment(ctx, repository, target, h.presentation.ClarificationCommentMarker(runID, packetVersion))
	if err != nil {
		return github.Comment{}, fmt.Errorf("find existing clarification questions on #%d: %w", target, err)
	}
	if strings.TrimSpace(comment.ID) != "" {
		if comment.Body != body {
			if err := h.issues.EditIssueComment(ctx, repository, comment.ID, body); err != nil {
				return github.Comment{}, fmt.Errorf("repair clarification questions on #%d: %w", target, err)
			}
			comment.Body = body
		}
		return comment, nil
	}
	created, err := h.issues.CreateIssueComment(ctx, repository, target, body)
	if err != nil {
		return github.Comment{}, fmt.Errorf("post clarification questions on #%d: %w", target, err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return github.Comment{}, fmt.Errorf("post clarification questions on #%d returned an empty comment id", target)
	}
	return created, nil
}

// Replay completes a question publication after a response-loss boundary by
// finding the marker before creating anything.
func (h clarificationHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload clarificationCommentEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	current, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during clarification replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during clarification replay", effect.RunID)
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindClarificationComment, *current); err != nil {
		return store.Run{}, err
	}
	comment, err := h.findOrCreateComment(ctx, payload.Repository, payload.Target, effect.RunID, payload.PacketVersion, payload.Body)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay clarification comment: %w", err)
	}
	current.ClarificationCommentID = comment.ID
	current.UpdatedAt = h.now().UTC()
	if err := h.projector.Save(ctx, runStore, *current); err != nil {
		return store.Run{}, fmt.Errorf("persist replayed clarification comment identity: %w", err)
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "clarification comment"); err != nil {
		return store.Run{}, err
	}
	return *current, nil
}
