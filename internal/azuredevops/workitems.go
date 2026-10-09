package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// workItemResponse is the work item projection needed by an issue snapshot.
type workItemResponse struct {
	ID     int `json:"id"`
	Fields struct {
		Title       string    `json:"System.Title"`
		Type        string    `json:"System.WorkItemType"`
		State       string    `json:"System.State"`
		Tags        string    `json:"System.Tags"`
		Description string    `json:"System.Description"`
		ChangedDate time.Time `json:"System.ChangedDate"`
	} `json:"fields"`
	MultilineFieldsFormat map[string]string `json:"multilineFieldsFormat"`
}

// Issue reads one work item as an issue snapshot.
func (c *Client) Issue(ctx context.Context, repository tracker.Repository, number int) (tracker.Issue, error) {
	if err := validateIssueTarget(repository, number); err != nil {
		return tracker.Issue{}, err
	}
	var response workItemResponse
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, fmt.Sprintf("wit/workitems/%d", number), nil, apiVersion)}, &response); err != nil {
		return tracker.Issue{}, fmt.Errorf("read work item %d: %w", number, err)
	}
	return c.issue(ctx, repository, response)
}

// issue converts one work item into the neutral issue snapshot. The state is
// "closed" when the state's category is Completed or Removed, so the mapping
// works for every process template.
func (c *Client) issue(ctx context.Context, repository tracker.Repository, response workItemResponse) (tracker.Issue, error) {
	category, err := c.stateCategory(ctx, repository, response.Fields.Type, response.Fields.State)
	if err != nil {
		return tracker.Issue{}, err
	}
	state := "open"
	if category == "Completed" || category == "Removed" {
		state = "closed"
	}
	return tracker.Issue{
		Number:    response.ID,
		Title:     response.Fields.Title,
		Body:      richText(response.Fields.Description, response.MultilineFieldsFormat["System.Description"]),
		State:     state,
		Labels:    splitTags(response.Fields.Tags),
		UpdatedAt: response.Fields.ChangedDate,
	}, nil
}

// stateCategory returns the category of one state of one work item type and
// caches the type's states.
func (c *Client) stateCategory(ctx context.Context, repository tracker.Repository, workItemType, state string) (string, error) {
	c.mu.Lock()
	states, ok := c.stateCategories[workItemType]
	c.mu.Unlock()
	if !ok {
		var response struct {
			Value []struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			} `json:"value"`
		}
		path := "wit/workitemtypes/" + url.PathEscape(workItemType) + "/states"
		if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, path, nil, apiVersion)}, &response); err != nil {
			return "", fmt.Errorf("read states of work item type %q: %w", workItemType, err)
		}
		states = make(map[string]string, len(response.Value))
		for _, value := range response.Value {
			states[value.Name] = value.Category
		}
		c.mu.Lock()
		if c.stateCategories == nil {
			c.stateCategories = map[string]map[string]string{}
		}
		c.stateCategories[workItemType] = states
		c.mu.Unlock()
	}
	category, ok := states[state]
	if !ok {
		return "", fmt.Errorf("work item type %q has no state %q", workItemType, state)
	}
	return category, nil
}

// validateIssueTarget rejects an incomplete repository or a non-positive
// work item id before any call.
func validateIssueTarget(repository tracker.Repository, number int) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if number <= 0 {
		return errors.New("work item id must be positive")
	}
	return nil
}

// eligibleQuery selects the project's work items that carry the agent-ready
// tag. State and authorization are checked after the read.
const eligibleQuery = "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project AND [System.Tags] CONTAINS '" + tracker.LabelAgentReady + "' ORDER BY [System.Id]"

// workItemBatchSize is the largest id list of one work item batch read.
const workItemBatchSize = 200

var _ tracker.IssuePoller = (*Client)(nil)

// ListEligibleIssues returns the open work items whose agent-ready tag an
// authorized user added, in ascending id order.
func (c *Client) ListEligibleIssues(ctx context.Context, repository tracker.Repository) ([]tracker.Issue, error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	var query struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if err := c.call(ctx, request{Method: "POST", URL: projectURL(repository, "wit/wiql", nil, apiVersion), Body: map[string]string{"query": eligibleQuery}}, &query); err != nil {
		return nil, fmt.Errorf("query eligible work items: %w", err)
	}
	ids := make([]int, 0, len(query.WorkItems))
	for _, item := range query.WorkItems {
		ids = append(ids, item.ID)
	}
	sort.Ints(ids)
	issues := make([]tracker.Issue, 0, len(ids))
	for start := 0; start < len(ids); start += workItemBatchSize {
		batch, err := c.workItems(ctx, repository, ids[start:min(start+workItemBatchSize, len(ids))])
		if err != nil {
			return nil, err
		}
		for _, response := range batch {
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

// workItems reads one batch of work items.
func (c *Client) workItems(ctx context.Context, repository tracker.Repository, ids []int) ([]workItemResponse, error) {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, strconv.Itoa(id))
	}
	var response struct {
		Value []workItemResponse `json:"value"`
	}
	query := url.Values{"ids": {strings.Join(values, ",")}, "errorPolicy": {"omit"}}
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "wit/workitems", query, apiVersion)}, &response); err != nil {
		return nil, fmt.Errorf("read eligible work items: %w", err)
	}
	return response.Value, nil
}

// eligibleIssue returns the issue snapshot of an open, agent-ready work item
// whose tag an authorized user added, or a zero issue otherwise.
func (c *Client) eligibleIssue(ctx context.Context, repository tracker.Repository, response workItemResponse) (tracker.Issue, error) {
	if response.ID <= 0 {
		return tracker.Issue{}, nil
	}
	issue, err := c.issue(ctx, repository, response)
	if err != nil {
		return tracker.Issue{}, err
	}
	if issue.State != "open" || !slices.Contains(issue.Labels, tracker.LabelAgentReady) {
		return tracker.Issue{}, nil
	}
	tagger, err := c.readyTagger(ctx, repository, issue.Number)
	if err != nil {
		return tracker.Issue{}, err
	}
	if !c.authorized(tagger) {
		return tracker.Issue{}, nil
	}
	return issue, nil
}

// updatePageSize is the page size of the work item update history.
const updatePageSize = 200

// readyTagger returns the identity of the last update that added the
// agent-ready tag to a work item.
func (c *Client) readyTagger(ctx context.Context, repository tracker.Repository, number int) (string, error) {
	tagger := ""
	for skip := 0; ; skip += updatePageSize {
		var response struct {
			Value []struct {
				RevisedBy identityRef `json:"revisedBy"`
				Fields    struct {
					Tags *struct {
						OldValue string `json:"oldValue"`
						NewValue string `json:"newValue"`
					} `json:"System.Tags"`
				} `json:"fields"`
			} `json:"value"`
		}
		query := url.Values{"$top": {strconv.Itoa(updatePageSize)}, "$skip": {strconv.Itoa(skip)}}
		if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, fmt.Sprintf("wit/workItems/%d/updates", number), query, apiVersion)}, &response); err != nil {
			return "", fmt.Errorf("read update history of work item %d: %w", number, err)
		}
		for _, update := range response.Value {
			tags := update.Fields.Tags
			if tags != nil && slices.Contains(splitTags(tags.NewValue), tracker.LabelAgentReady) && !slices.Contains(splitTags(tags.OldValue), tracker.LabelAgentReady) {
				tagger = update.RevisedBy.UniqueName
			}
		}
		if len(response.Value) < updatePageSize {
			return tagger, nil
		}
	}
}
