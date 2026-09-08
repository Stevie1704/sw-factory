package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// issueProjection makes the two GitHub projections of a run — its state label
// and its editable status comment — converge on a persisted run revision. It
// is the shared apply path behind every kind that advances workflow state.
type issueProjection struct {
	issues       IssueClient
	presentation RunPresentation
}

// applyStateTransition makes the two GitHub projections converge on a
// persisted run revision. Reads before each mutation recognize an effect that
// completed just before a process interruption.
func (p issueProjection) applyStateTransition(ctx context.Context, next *store.Run, transition StateTransition) error {
	if next == nil {
		return errors.New("state transition run is required")
	}
	issue := transition.Issue
	if p.issues == nil {
		return errors.New("GitHub client is required for state transition")
	}
	observed, err := p.issues.Issue(ctx, transition.Repository, next.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue before state transition: %w", err)
	}
	if observed.Number != 0 {
		issue = observed
	}
	desiredLabels := p.presentation.StateLabels(issue.Labels, next.Status)
	if !sameStringSlice(issue.Labels, desiredLabels) {
		if err := p.issues.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, desiredLabels); err != nil {
			return fmt.Errorf("set issue #%d state: %w", next.IssueNumber, err)
		}
	}

	marker := p.presentation.StatusCommentMarker(next.ID)
	comment, err := p.issues.FindStatusComment(ctx, transition.Repository, next.IssueNumber, marker)
	if err != nil {
		return fmt.Errorf("find status comment during state transition: %w", err)
	}
	if transition.CreateComment {
		if strings.TrimSpace(comment.ID) != "" {
			next.StatusCommentID = comment.ID
			if comment.Body != p.presentation.StatusCommentBody(*next) {
				if err := p.issues.EditIssueComment(ctx, transition.Repository, comment.ID, p.presentation.StatusCommentBody(*next)); err != nil {
					return fmt.Errorf("repair existing status comment: %w", err)
				}
			}
			return nil
		}
		created, err := p.issues.CreateIssueComment(ctx, transition.Repository, next.IssueNumber, p.presentation.StatusCommentBody(*next))
		if err != nil {
			return fmt.Errorf("create status comment: %w", err)
		}
		if strings.TrimSpace(created.ID) == "" {
			return errors.New("create status comment returned an empty comment id")
		}
		next.StatusCommentID = created.ID
		return nil
	}
	if strings.TrimSpace(next.StatusCommentID) == "" {
		if strings.TrimSpace(comment.ID) == "" {
			return errors.New("active run has no recoverable status comment")
		}
		next.StatusCommentID = comment.ID
	}
	if comment.ID == next.StatusCommentID && comment.Body == p.presentation.StatusCommentBody(*next) {
		return nil
	}
	if err := p.issues.EditIssueComment(ctx, transition.Repository, next.StatusCommentID, p.presentation.StatusCommentBody(*next)); err != nil {
		return fmt.Errorf("edit status comment: %w", err)
	}
	return nil
}

// applyStatusComment observes the marker-owned status comment before editing
// it. A matching body is already complete, so a replay after an ambiguous
// GitHub response performs no second mutation.
func (p issueProjection) applyStatusComment(ctx context.Context, payload statusCommentEffectPayload) error {
	if p.issues == nil {
		return errors.New("GitHub client is required for status comment effect")
	}
	if strings.TrimSpace(payload.Next.StatusCommentID) == "" {
		return errors.New("status comment effect has no comment identity")
	}
	comment, err := p.issues.FindStatusComment(ctx, payload.Repository, payload.Next.IssueNumber, p.presentation.StatusCommentMarker(payload.Next.ID))
	if err != nil {
		return fmt.Errorf("find status comment for replay: %w", err)
	}
	body := p.presentation.StatusCommentBody(payload.Next)
	if comment.ID == payload.Next.StatusCommentID && comment.Body == body {
		return nil
	}
	if comment.ID != "" && comment.ID != payload.Next.StatusCommentID {
		if comment.Body == body {
			return nil
		}
		return fmt.Errorf("status comment identity changed from %q to %q", payload.Next.StatusCommentID, comment.ID)
	}
	if err := p.issues.EditIssueComment(ctx, payload.Repository, payload.Next.StatusCommentID, body); err != nil {
		return fmt.Errorf("edit status comment for replay: %w", err)
	}
	return nil
}
