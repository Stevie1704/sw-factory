package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// classifyPlanError maps a failed az rest call to a typed planning error when
// the response proves that Azure DevOps made no change: Unauthorized and
// Forbidden are permission errors, and Bad Request and Not Found are invalid
// mappings, for example a rule violation or an unknown type. A timeout is
// ambiguous. Every other failure stays unclassified, so its outcome is
// unknown. az rest reports a failed response as "<reason>(<body>)". A body
// too large for one az argument is never sent, so it is an invalid mapping.
func classifyPlanError(err error) error {
	var timeout *hostcmd.TimeoutError
	if errors.As(err, &timeout) {
		return &tracker.PlanError{Kind: tracker.PlanErrorAmbiguous, Err: err}
	}
	if errors.Is(err, errRequestBodyTooLarge) {
		return &tracker.PlanError{Kind: tracker.PlanErrorInvalidMapping, Err: err}
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "Unauthorized(") || strings.Contains(message, "Forbidden("):
		return &tracker.PlanError{Kind: tracker.PlanErrorPermission, Err: err}
	case strings.Contains(message, "Bad Request(") || strings.Contains(message, "Not Found("):
		return &tracker.PlanError{Kind: tracker.PlanErrorInvalidMapping, Err: err}
	}
	return err
}

// patchOperation is one JSON patch operation of a work item write.
type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// CreatePlanItem creates exactly the rendered work item: its type, title,
// Markdown description, correlation tag, and configured fields. It sets no
// relation, never a factory state tag, and never an execution state.
func (c *Client) CreatePlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	if err := validatePlanRepository(repository); err != nil {
		return tracker.PlanItemRef{}, err
	}
	if err := validatePlanItem(item); err != nil {
		return tracker.PlanItemRef{}, err
	}
	if strings.TrimSpace(item.Type) == "" || !slices.Contains(item.Labels, correlationTag(item.Correlation)) {
		return tracker.PlanItemRef{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the item has no type or no correlation tag", item.NodeID)
	}
	patch := createPatch(item)
	var response struct {
		ID int `json:"id"`
	}
	req := request{Method: "POST", URL: projectURL(repository, "wit/workitems/"+url.PathEscape("$"+item.Type), nil, apiVersion), Body: patch, ContentType: "application/json-patch+json"}
	if err := c.call(ctx, req, &response); err != nil {
		return tracker.PlanItemRef{}, fmt.Errorf("create %s for node %s: %w", item.Type, item.NodeID, classifyPlanError(err))
	}
	if response.ID <= 0 {
		return tracker.PlanItemRef{}, fmt.Errorf("create %s for node %s: the response has no work item ID", item.Type, item.NodeID)
	}
	return workItemRef(repository, response.ID), nil
}

// createPatch returns the JSON patch that creates item: its title, Markdown
// description, correlation tag, and configured fields.
func createPatch(item tracker.RenderedPlanItem) []patchOperation {
	patch := []patchOperation{
		{Op: "add", Path: "/fields/System.Title", Value: item.Title},
		{Op: "add", Path: "/fields/System.Description", Value: item.Body},
		{Op: "add", Path: "/multilineFieldsFormat/System.Description", Value: "Markdown"},
		{Op: "add", Path: "/fields/System.Tags", Value: joinTags(item.Labels)},
	}
	for _, name := range slices.Sorted(maps.Keys(item.Fields)) {
		patch = append(patch, patchOperation{Op: "add", Path: "/fields/" + name, Value: item.Fields[name]})
	}
	return patch
}

// workItemRef returns the reference of one work item with its web URL.
func workItemRef(repository tracker.Repository, id int) tracker.PlanItemRef {
	return tracker.PlanItemRef{
		ExternalID: strconv.Itoa(id),
		URL:        serviceURL + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Project) + "/_workitems/edit/" + strconv.Itoa(id),
	}
}

// FindPlanItems returns every work item of the project, in any state, that
// carries the correlation tag, in ascending ID order. WIQL reads the work
// item store, not a search index, so it sees a just-created item. The tags
// of each match are read back and compared exactly.
func (c *Client) FindPlanItems(ctx context.Context, repository tracker.Repository, correlation string) ([]tracker.PlanItemRef, error) {
	if err := validatePlanRepository(repository); err != nil {
		return nil, err
	}
	if strings.TrimSpace(correlation) == "" {
		return nil, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "a correlation key is required")
	}
	tag := correlationTag(correlation)
	query := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project AND [System.Tags] CONTAINS '" +
		strings.ReplaceAll(tag, "'", "''") + "' ORDER BY [System.Id]"
	var result struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if err := c.call(ctx, request{Method: "POST", URL: projectURL(repository, "wit/wiql", nil, apiVersion), Body: map[string]string{"query": query}}, &result); err != nil {
		return nil, fmt.Errorf("query work items by correlation %s: %w", correlation, err)
	}
	ids := make([]int, 0, len(result.WorkItems))
	for _, item := range result.WorkItems {
		ids = append(ids, item.ID)
	}
	slices.Sort(ids)
	refs := []tracker.PlanItemRef{}
	for start := 0; start < len(ids); start += workItemBatchSize {
		batch, err := c.workItems(ctx, repository, ids[start:min(start+workItemBatchSize, len(ids))])
		if err != nil {
			return nil, err
		}
		for _, item := range batch {
			if slices.ContainsFunc(splitTags(item.Fields.Tags), func(value string) bool { return strings.EqualFold(value, tag) }) {
				refs = append(refs, workItemRef(repository, item.ID))
			}
		}
	}
	return refs, nil
}

// relationResponse is the relation projection of one work item.
type relationResponse struct {
	Relations []struct {
		Rel string `json:"rel"`
		URL string `json:"url"`
	} `json:"relations"`
}

// CreatePlanRelationship adds one native link on the From work item: a
// parent link to To, or a predecessor link that makes To the item that must
// be done first.
func (c *Client) CreatePlanRelationship(ctx context.Context, repository tracker.Repository, relationship tracker.PlanRelationship) error {
	from, to, link, err := planLink(repository, relationship)
	if err != nil {
		return err
	}
	patch := []patchOperation{{Op: "add", Path: "/relations/-", Value: map[string]string{"rel": link, "url": workItemAPIURL(repository, to)}}}
	req := request{Method: "PATCH", URL: projectURL(repository, fmt.Sprintf("wit/workitems/%d", from), nil, apiVersion), Body: patch, ContentType: "application/json-patch+json"}
	if err := c.call(ctx, req, nil); err != nil {
		return fmt.Errorf("link work item %d to %d: %w", from, to, classifyPlanError(err))
	}
	return nil
}

// FindPlanRelationship reports whether the From work item already has the
// link to To.
func (c *Client) FindPlanRelationship(ctx context.Context, repository tracker.Repository, relationship tracker.PlanRelationship) (bool, error) {
	from, to, link, err := planLink(repository, relationship)
	if err != nil {
		return false, err
	}
	var response relationResponse
	query := url.Values{"$expand": {"relations"}}
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, fmt.Sprintf("wit/workitems/%d", from), query, apiVersion)}, &response); err != nil {
		return false, fmt.Errorf("read links of work item %d: %w", from, err)
	}
	for _, relation := range response.Relations {
		if relation.Rel == link && linkedWorkItem(relation.URL) == to {
			return true, nil
		}
	}
	return false, nil
}

// planLink validates one relationship and returns its work item IDs and
// link type. Only native links exist on Azure DevOps.
func planLink(repository tracker.Repository, relationship tracker.PlanRelationship) (int, int, string, error) {
	if err := validatePlanRepository(repository); err != nil {
		return 0, 0, "", err
	}
	link, ok := linkTypes[relationship.Kind]
	if !ok || relationship.Mode != tracker.PlanRelationshipNative {
		return 0, 0, "", tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "Azure DevOps supports only native %s links, not %q", relationship.Kind, relationship.Mode)
	}
	from, err := workItemID(relationship.From)
	if err != nil {
		return 0, 0, "", err
	}
	to, err := workItemID(relationship.To)
	if err != nil {
		return 0, 0, "", err
	}
	return from, to, link, nil
}

// workItemAPIURL returns the API URL of a work item that a relation names.
func workItemAPIURL(repository tracker.Repository, id int) string {
	return serviceURL + url.PathEscape(repository.Owner) + "/_apis/wit/workItems/" + strconv.Itoa(id)
}

// linkedWorkItem returns the work item ID at the end of a relation URL, or
// zero.
func linkedWorkItem(value string) int {
	parsed, err := url.Parse(value)
	if err != nil {
		return 0
	}
	segments := strings.Split(strings.TrimRight(parsed.Path, "/"), "/")
	if len(segments) < 2 || !strings.EqualFold(segments[len(segments)-2], "workItems") {
		return 0
	}
	id, err := strconv.Atoi(segments[len(segments)-1])
	if err != nil {
		return 0
	}
	return id
}
