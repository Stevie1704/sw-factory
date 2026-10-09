package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var _ tracker.Client = (*Client)(nil)
var _ tracker.CommentReader = (*Client)(nil)

// CreateLabel succeeds without a call: Azure DevOps creates a tag when a work
// item first uses it.
func (c *Client) CreateLabel(context.Context, tracker.Repository, tracker.Label) error {
	return nil
}

// ReplaceIssueLabels replaces the complete tag list of one work item.
func (c *Client) ReplaceIssueLabels(ctx context.Context, repository tracker.Repository, number int, labels []string) error {
	if err := validateIssueTarget(repository, number); err != nil {
		return err
	}
	patch := []map[string]string{{"op": "add", "path": "/fields/System.Tags", "value": joinTags(labels)}}
	req := request{Method: "PATCH", URL: projectURL(repository, fmt.Sprintf("wit/workitems/%d", number), nil, apiVersion), Body: patch, ContentType: "application/json-patch+json"}
	if err := c.call(ctx, req, nil); err != nil {
		return fmt.Errorf("replace tags on work item %d: %w", number, err)
	}
	return nil
}

// workItemCommentResponse is the work item comment projection.
type workItemCommentResponse struct {
	ID           int         `json:"id"`
	Text         string      `json:"text"`
	Format       string      `json:"format"`
	IsDeleted    bool        `json:"isDeleted"`
	CreatedBy    identityRef `json:"createdBy"`
	CreatedDate  time.Time   `json:"createdDate"`
	ModifiedDate time.Time   `json:"modifiedDate"`
}

// comment converts one work item comment into the neutral model. The
// identity starts with the creation time, so it sorts with pull-request
// comment identities (ADR 0019).
func (r workItemCommentResponse) comment(number int) tracker.Comment {
	updated := r.ModifiedDate
	if updated.IsZero() {
		updated = r.CreatedDate
	}
	return tracker.Comment{ID: eventID(r.CreatedDate, "w", number, "c", r.ID), Body: richText(r.Text, r.Format), Author: r.CreatedBy.UniqueName, UpdatedAt: updated}
}

// IssueComments lists the comments of one work item in creation order.
func (c *Client) IssueComments(ctx context.Context, repository tracker.Repository, number int) ([]tracker.Comment, error) {
	responses, err := c.workItemComments(ctx, repository, number)
	if err != nil {
		return nil, err
	}
	comments := make([]tracker.Comment, 0, len(responses))
	for _, response := range responses {
		comments = append(comments, response.comment(number))
	}
	return comments, nil
}

// commentPageSize is the page size of the work item comment list.
const commentPageSize = 200

// workItemComments reads every page of the comments of one work item,
// without deleted comments.
func (c *Client) workItemComments(ctx context.Context, repository tracker.Repository, number int) ([]workItemCommentResponse, error) {
	if err := validateIssueTarget(repository, number); err != nil {
		return nil, err
	}
	comments := make([]workItemCommentResponse, 0)
	token := ""
	for {
		query := url.Values{"$top": {fmt.Sprint(commentPageSize)}, "order": {"asc"}}
		if token != "" {
			query.Set("continuationToken", token)
		}
		var response struct {
			Comments          []workItemCommentResponse `json:"comments"`
			ContinuationToken string                    `json:"continuationToken"`
		}
		if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, fmt.Sprintf("wit/workItems/%d/comments", number), query, commentAPIVersion)}, &response); err != nil {
			return nil, fmt.Errorf("list comments on work item %d: %w", number, err)
		}
		for _, comment := range response.Comments {
			if !comment.IsDeleted {
				comments = append(comments, comment)
			}
		}
		if response.ContinuationToken == "" || response.ContinuationToken == token {
			return comments, nil
		}
		token = response.ContinuationToken
	}
}

// CreateIssueComment posts one Markdown comment on a work item.
func (c *Client) CreateIssueComment(ctx context.Context, repository tracker.Repository, number int, body string) (tracker.Comment, error) {
	if err := validateIssueTarget(repository, number); err != nil {
		return tracker.Comment{}, err
	}
	var response workItemCommentResponse
	req := request{Method: "POST", URL: projectURL(repository, fmt.Sprintf("wit/workItems/%d/comments", number), markdownFormat(), commentAPIVersion), Body: map[string]string{"text": body}}
	if err := c.call(ctx, req, &response); err != nil {
		return tracker.Comment{}, fmt.Errorf("create comment on work item %d: %w", number, err)
	}
	comment := response.comment(number)
	comment.Body = body
	return comment, nil
}

// FindStatusComment returns the comment of the authenticated identity that
// contains marker, or a zero comment. It matches the stored text, so an
// HTML conversion cannot remove the marker.
func (c *Client) FindStatusComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error) {
	if strings.TrimSpace(marker) == "" {
		return tracker.Comment{}, errors.New("status comment marker is required")
	}
	coordinator, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return tracker.Comment{}, err
	}
	responses, err := c.workItemComments(ctx, repository, number)
	if err != nil {
		return tracker.Comment{}, err
	}
	for _, response := range responses {
		if strings.Contains(response.Text, marker) && strings.EqualFold(strings.TrimSpace(response.CreatedBy.UniqueName), coordinator) {
			return response.comment(number), nil
		}
	}
	return tracker.Comment{}, nil
}

// EditIssueComment replaces the text of one work item comment. The identity
// names the work item, because comment ids are unique only per work item.
func (c *Client) EditIssueComment(ctx context.Context, repository tracker.Repository, commentID string, body string) error {
	number, okNumber := eventIDPart(commentID, "w")
	id, okID := eventIDPart(commentID, "c")
	if !okNumber || !okID {
		return fmt.Errorf("%q is not an Azure DevOps work item comment identity", commentID)
	}
	if err := validateIssueTarget(repository, number); err != nil {
		return err
	}
	req := request{Method: "PATCH", URL: projectURL(repository, fmt.Sprintf("wit/workItems/%d/comments/%d", number, id), markdownFormat(), commentAPIVersion), Body: map[string]string{"text": body}}
	if err := c.call(ctx, req, nil); err != nil {
		return fmt.Errorf("edit comment %d on work item %d: %w", id, number, err)
	}
	return nil
}

// markdownFormat is the query that stores a comment as Markdown, so the
// factory's Markdown status text and HTML-comment markers stay intact.
func markdownFormat() url.Values {
	return url.Values{"format": {"markdown"}}
}
