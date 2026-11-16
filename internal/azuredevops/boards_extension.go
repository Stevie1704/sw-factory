package azuredevops

// This file holds the Boards operations of the Azure DevOps CLI extension
// route (ADR 0030): dedicated az boards commands where they exist, and
// az devops invoke through the verified area/resource pairs otherwise.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// extensionAPIVersion is the API version of the released resources that the
// extension route reaches through az devops invoke.
const extensionAPIVersion = "7.1"

// extensionCommentAPIVersion is the work item comment API version on the
// extension route. Invoke cannot parse the resource version of
// commentAPIVersion; "7.1-preview" is a documented version form, but which
// resource version the service then selects is not verified live (ADR 0030),
// so every comment write checks the stored format.
const extensionCommentAPIVersion = "7.1-preview"

// extensionCommentRequest addresses the comments of one work item, or one
// comment when commentID is positive, through az devops invoke.
func extensionCommentRequest(repository tracker.Repository, number int, method string, query url.Values, body any) invokeRequest {
	route := projectRoute(repository, "workItemId", strconv.Itoa(number))
	parameters := map[string]string{}
	for key := range query {
		parameters[key] = query.Get(key)
	}
	return invokeRequest{Area: "wit", Resource: "comments", Method: method, APIVersion: extensionCommentAPIVersion, Route: route, Query: parameters, Body: body}
}

// extensionWriteComment creates a Markdown comment on a work item, or edits
// comment commentID when it is positive, and refuses a result that the
// service did not store as Markdown, because status markers and Markdown
// text would not survive an HTML conversion.
func (c *Client) extensionWriteComment(ctx context.Context, repository tracker.Repository, number, commentID int, body string) (tracker.Comment, error) {
	method, action := "POST", "create comment"
	req := extensionCommentRequest(repository, number, method, markdownFormat(), map[string]string{"text": body})
	if commentID > 0 {
		req.Method, action = "PATCH", fmt.Sprintf("edit comment %d", commentID)
		req.Route["commentId"] = strconv.Itoa(commentID)
	}
	var response workItemCommentResponse
	if err := c.invoke(ctx, repository.Owner, req, &response); err != nil {
		return tracker.Comment{}, fmt.Errorf("%s on work item %d: %w", action, number, err)
	}
	if !strings.EqualFold(strings.TrimSpace(response.Format), "markdown") {
		return tracker.Comment{}, fmt.Errorf("%s on work item %d: Azure DevOps stored the comment as %q, not Markdown; the comment API version %s is not verified for Markdown comments", action, number, response.Format, extensionCommentAPIVersion)
	}
	comment := response.comment(number)
	comment.Body = body
	return comment, nil
}

// extensionFindStatusComment recovers the status comment by its marker
// alone. The extension route cannot read the signed-in identity (ADR 0030),
// so it cannot apply the author check of ADR 0019; it accepts exactly one
// live comment with the marker and refuses more than one instead of
// guessing, so a copied marker never redirects the status comment and never
// causes a duplicate.
func (c *Client) extensionFindStatusComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error) {
	responses, err := c.workItemComments(ctx, repository, number)
	if err != nil {
		return tracker.Comment{}, err
	}
	matches := []workItemCommentResponse{}
	for _, response := range responses {
		if strings.Contains(response.Text, marker) {
			matches = append(matches, response)
		}
	}
	switch len(matches) {
	case 0:
		return tracker.Comment{}, nil
	case 1:
		return matches[0].comment(number), nil
	default:
		return tracker.Comment{}, fmt.Errorf("work item %d has %d comments with the status marker; delete the copies that Factory did not write", number, len(matches))
	}
}

// projectRoute returns the route parameters that select the registered
// project, plus optional key/value pairs such as "id", "42".
func projectRoute(repository tracker.Repository, pairs ...string) map[string]string {
	route := map[string]string{"project": repository.Project}
	for index := 0; index+1 < len(pairs); index += 2 {
		route[pairs[index]] = pairs[index+1]
	}
	return route
}

// extensionWorkItems reads work items through the project-scoped
// workItemsBatch resource. Missing items and items of another project are
// left out, so a work item number never reaches into another project.
func (c *Client) extensionWorkItems(ctx context.Context, repository tracker.Repository, ids []int) ([]workItemResponse, error) {
	var response struct {
		Value []*workItemResponse `json:"value"`
	}
	req := invokeRequest{
		Area: "wit", Resource: "workItemsBatch", Method: "POST", APIVersion: extensionAPIVersion,
		Route: projectRoute(repository),
		Body:  map[string]any{"ids": ids, "errorPolicy": "omit"},
	}
	if err := c.invoke(ctx, repository.Owner, req, &response); err != nil {
		return nil, fmt.Errorf("read work items: %w", err)
	}
	items := make([]workItemResponse, 0, len(response.Value))
	for _, item := range response.Value {
		if item != nil && item.ID > 0 && strings.EqualFold(item.Fields.TeamProject, repository.Project) {
			items = append(items, *item)
		}
	}
	return items, nil
}

// extensionWorkItem reads one work item of the registered project.
func (c *Client) extensionWorkItem(ctx context.Context, repository tracker.Repository, number int) (workItemResponse, error) {
	items, err := c.extensionWorkItems(ctx, repository, []int{number})
	if err != nil {
		return workItemResponse{}, err
	}
	for _, item := range items {
		if item.ID == number {
			return item, nil
		}
	}
	return workItemResponse{}, fmt.Errorf("work item %d does not exist in project %s or cannot be read", number, repository.Project)
}

var _ tracker.RepositoryAssignmentValidator = (*Client)(nil)

// ClaimRepositoryAssignment validates, right before a claim, that issue
// carries exactly one factory-repo tag naming the registered repository's
// GUID, and returns the identity to freeze with the run. The az rest route
// needs no assignment and returns nil without a call.
func (c *Client) ClaimRepositoryAssignment(ctx context.Context, repository tracker.Repository, issue tracker.Issue) (*tracker.RepositoryAssignment, error) {
	if !c.extensionRoute() {
		return nil, nil
	}
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	if err := c.validateAssignment(ctx, repository, issue.Labels, identity); err != nil {
		return nil, fmt.Errorf("work item %d cannot be claimed by this installation: %w", issue.Number, err)
	}
	return &tracker.RepositoryAssignment{
		Organization: identity.Organization,
		ProjectID:    identity.ProjectID,
		Project:      identity.Project,
		RepositoryID: identity.ID,
		Repository:   identity.Name,
	}, nil
}

// wiqlString quotes value as a WIQL string literal.
func wiqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// extensionEligibleIssues is ListEligibleIssues on the extension route. The
// WIQL names the project explicitly, because az boards query sends no
// project context for @project, and filters by the assignment tag only as
// an optimization: every listed work item passes the exact assignment check,
// so overlapping installations never list each other's work.
func (c *Client) extensionEligibleIssues(ctx context.Context, repository tracker.Repository) ([]tracker.Issue, error) {
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	wiql := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = " + wiqlString(repository.Project) +
		" AND [System.Tags] CONTAINS " + wiqlString(tracker.LabelAgentReady) +
		" AND [System.Tags] CONTAINS " + wiqlString(RepositoryAssignmentTag(identity.ID)) +
		" ORDER BY [System.Id]"
	var found []struct {
		ID int `json:"id"`
	}
	if err := c.extension(ctx, repository.Owner, []string{"boards", "query", "--wiql", wiql, "--project", repository.Project}, &found); err != nil {
		return nil, fmt.Errorf("query eligible work items: %w", err)
	}
	ids := make([]int, 0, len(found))
	for _, item := range found {
		ids = append(ids, item.ID)
	}
	sort.Ints(ids)
	issues := make([]tracker.Issue, 0, len(ids))
	for start := 0; start < len(ids); start += workItemBatchSize {
		batch, err := c.extensionWorkItems(ctx, repository, ids[start:min(start+workItemBatchSize, len(ids))])
		if err != nil {
			return nil, err
		}
		for _, response := range batch {
			if c.validateAssignment(ctx, repository, splitTags(response.Fields.Tags), identity) != nil {
				continue
			}
			issue, err := c.eligibleIssue(ctx, repository, response)
			if err != nil {
				return nil, err
			}
			if issue.Number > 0 {
				issues = append(issues, issue)
			}
		}
	}
	sort.SliceStable(issues, func(left, right int) bool { return issues[left].Number < issues[right].Number })
	return issues, nil
}

// metadataTag reports whether tag is a repository assignment or plan
// correlation tag. Neither is a run-state tag, and every lifecycle tag update
// must keep both (ADR 0030 decision 10).
func metadataTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	return IsRepositoryAssignmentTag(tag) || (len(tag) >= len(correlationTagPrefix) && strings.EqualFold(tag[:len(correlationTagPrefix)], correlationTagPrefix))
}

// keepMetadataTags appends to labels every assignment and correlation tag of
// current that labels does not already hold.
func keepMetadataTags(labels, current []string) []string {
	result := append([]string(nil), labels...)
	for _, tag := range current {
		if metadataTag(tag) && !slices.ContainsFunc(result, func(label string) bool { return strings.EqualFold(strings.TrimSpace(label), tag) }) {
			result = append(result, tag)
		}
	}
	return result
}

// extensionReplaceIssueLabels replaces the tag list of one work item with
// az boards work-item update. The command is organization-scoped, so the
// work item is first read inside the registered project; the read also
// supplies the assignment and correlation tags that the write keeps.
func (c *Client) extensionReplaceIssueLabels(ctx context.Context, repository tracker.Repository, number int, labels []string) error {
	current, err := c.extensionWorkItem(ctx, repository, number)
	if err != nil {
		return fmt.Errorf("replace tags on work item %d: %w", number, err)
	}
	tags := joinTags(keepMetadataTags(labels, splitTags(current.Fields.Tags)))
	args := []string{"boards", "work-item", "update", "--id", strconv.Itoa(number), "--fields", "System.Tags=" + tags}
	if err := c.extension(ctx, repository.Owner, args, nil); err != nil {
		return fmt.Errorf("replace tags on work item %d: %w", number, err)
	}
	return nil
}
