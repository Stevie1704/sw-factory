package azuredevops

// This file holds the plan writes of the Azure DevOps CLI extension route
// (ADR 0030): az boards work-item create, az boards query, and az boards
// work-item relation add and show.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// extensionNoWriteTexts are the azure-devops extension's error texts that
// prove a plan write made no change: the field rule refusal of az boards
// work-item create (work_item.py _handle_vsts_service_error, raised for a
// RuleValidationException of the service), and the checks of az boards
// work-item relation add that run before its update (relations.py). Every
// other extension failure keeps an unknown outcome.
var extensionNoWriteTexts = []string{
	"To specify a value for this field, use the --field argument",
	"argument to supply this value.",
	"RuleValidationException for FieldReferenceName:",
	"--relation-type is not valid",
	"Id(s) supplied in --target-id is not valid",
}

// checkAssignmentTags refuses an item whose repository assignment tags do
// not match its target: an executable item carries exactly the tag of its
// target, and an item without a target carries none.
func checkAssignmentTags(item tracker.RenderedPlanItem) error {
	found := assignmentTags(item.Labels)
	switch {
	case item.TargetRepository == nil && len(found) == 0:
		return nil
	case item.TargetRepository != nil && canonicalGUID.MatchString(item.TargetRepository.ID) &&
		len(found) == 1 && found[0] == RepositoryAssignmentTag(item.TargetRepository.ID):
		return nil
	}
	return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the repository assignment tags %q do not match the target repository of the approved item", item.NodeID, found)
}

// assignmentTags returns the repository assignment tags among labels.
func assignmentTags(labels []string) []string {
	found := []string{}
	for _, label := range labels {
		if IsRepositoryAssignmentTag(strings.TrimSpace(label)) {
			found = append(found, strings.TrimSpace(label))
		}
	}
	return found
}

var _ tracker.PlanItemVerifier = (*Client)(nil)

// VerifyPlanItem accepts an existing work item found by correlation only
// when it is in the registered project, has the approved type, and carries
// exactly the approved repository assignment: the target's one
// factory-repo tag, or none for an item without a target (ADR 0030
// decision 10). It never changes the item. The az rest route has no
// assignment and accepts the item as before.
func (c *Client) VerifyPlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem, ref tracker.PlanItemRef) error {
	return c.backend().verifyPlanItem(ctx, repository, item, ref)
}

// verifyPlanItem accepts every item: the az rest route has no repository
// assignment to verify.
func (restBackend) verifyPlanItem(context.Context, tracker.Repository, tracker.RenderedPlanItem, tracker.PlanItemRef) error {
	return nil
}

// verifyPlanItem checks the project, type, and repository assignment of an
// item found by correlation.
func (c extensionBackend) verifyPlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem, ref tracker.PlanItemRef) error {
	id, err := workItemID(ref)
	if err != nil {
		return err
	}
	current, err := c.workItem(ctx, repository, id)
	if err != nil {
		return err
	}
	if !strings.EqualFold(current.Fields.Type, item.Type) {
		return fmt.Errorf("work item %d is a %s, not the approved %s", id, current.Fields.Type, item.Type)
	}
	found := assignmentTags(splitTags(current.Fields.Tags))
	want := []string{}
	if item.TargetRepository != nil {
		want = append(want, RepositoryAssignmentTag(item.TargetRepository.ID))
	}
	if !slices.EqualFunc(found, want, strings.EqualFold) {
		return fmt.Errorf("work item %d has the repository assignment %q, but the approved item has %q; Factory neither adopts nor retargets it", id, found, want)
	}
	return nil
}

// createPlanItem creates the rendered item with az boards work-item create
// in the registered project: its type, title, description, correlation and
// assignment tags, and configured fields, nothing else.
func (c extensionBackend) createPlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	if err := checkAssignmentTags(item); err != nil {
		return tracker.PlanItemRef{}, err
	}
	args, err := extensionCreateArgs(repository, item)
	if err != nil {
		return tracker.PlanItemRef{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: %v", item.NodeID, err)
	}
	var response struct {
		ID int `json:"id"`
	}
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return tracker.PlanItemRef{}, fmt.Errorf("create %s for node %s: %w", item.Type, item.NodeID, classifyPlanError(err))
	}
	if response.ID <= 0 {
		return tracker.PlanItemRef{}, fmt.Errorf("create %s for node %s: the response has no work item ID", item.Type, item.NodeID)
	}
	return workItemRef(repository, response.ID), nil
}

// findPlanItems returns every work item of the registered project, in any
// state, whose tags hold the correlation tag exactly. az boards query sends
// no project context, so the WIQL names the project; it returns at most
// 1000 items, which a unique correlation tag does not reach.
func (c extensionBackend) findPlanItems(ctx context.Context, repository tracker.Repository, correlation string) ([]tracker.PlanItemRef, error) {
	tag := correlationTag(correlation)
	wiql := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = " + wiqlString(repository.Project) +
		" AND [System.Tags] CONTAINS " + wiqlString(tag) + " ORDER BY [System.Id]"
	var found []struct {
		ID int `json:"id"`
	}
	if err := c.extension(ctx, repository.Owner, []string{"boards", "query", "--wiql", wiql, "--project", repository.Project}, &found); err != nil {
		return nil, fmt.Errorf("query work items by correlation tag %s: %w", tag, err)
	}
	ids := make([]int, 0, len(found))
	for _, item := range found {
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

// itemRelation is one relation of az boards work-item relation add or show,
// whose rel is the link type's friendly name.
type itemRelation struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// relationItem is the relation projection of az boards work-item relation
// add and show.
type relationItem struct {
	ID        int            `json:"id"`
	Relations []itemRelation `json:"relations"`
}

// has reports whether the item has a relation named name to work item to.
func (r relationItem) has(name string, to int) bool {
	return slices.ContainsFunc(r.Relations, func(relation itemRelation) bool {
		return strings.EqualFold(relation.Rel, name) && linkedWorkItem(relation.URL) == to
	})
}

// linkName returns the friendly name of the link type reference.
func (c extensionBackend) linkName(ctx context.Context, repository tracker.Repository, reference string) (string, error) {
	types, err := c.listLinkTypes(ctx, repository)
	if err != nil {
		return "", fmt.Errorf("read the link types of %s: %w", repository.Owner, classifyPlanError(err))
	}
	return linkTypeName(types, reference)
}

// createPlanRelationship adds the link on the From work item with az
// boards work-item relation add --id <from> --target-id <to>: for a
// parent link the Parent link type, so To becomes From's parent; for a
// dependency the Predecessor link type, so To must be done first. The
// command reads the item back after its update; a response without the
// relation is an unknown outcome that the lookup verifies.
func (c extensionBackend) createPlanRelationship(ctx context.Context, repository tracker.Repository, from, to int, reference string) error {
	name, err := c.linkName(ctx, repository, reference)
	if err != nil {
		return err
	}
	args := []string{"boards", "work-item", "relation", "add", "--id", strconv.Itoa(from), "--relation-type", name, "--target-id", strconv.Itoa(to)}
	var response relationItem
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return fmt.Errorf("link work item %d to %d: %w", from, to, classifyPlanError(err))
	}
	if response.ID != from || !response.has(name, to) {
		return fmt.Errorf("link work item %d to %d: the response does not show the %s link", from, to, name)
	}
	return nil
}

// findPlanRelationship reports whether the From work item already has the
// named link to To, read with az boards work-item relation show.
func (c extensionBackend) findPlanRelationship(ctx context.Context, repository tracker.Repository, from, to int, reference string) (bool, error) {
	name, err := c.linkName(ctx, repository, reference)
	if err != nil {
		return false, err
	}
	var response relationItem
	if err := c.extension(ctx, repository.Owner, []string{"boards", "work-item", "relation", "show", "--id", strconv.Itoa(from)}, &response); err != nil {
		return false, fmt.Errorf("read links of work item %d: %w", from, err)
	}
	if response.ID != from {
		return false, fmt.Errorf("read links of work item %d: the response names work item %d", from, response.ID)
	}
	return response.has(name, to), nil
}
