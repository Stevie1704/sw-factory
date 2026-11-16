package azuredevops_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Extension-route routing keys of the Boards operations (ADR 0030).
const (
	routeWorkItemsBatch = "devops invoke POST wit/workItemsBatch"
	routeTypeStates     = "devops invoke GET wit/workItemTypeStates"
	routeUpdates        = "devops invoke GET wit/updates"
	routeCommentsList   = "devops invoke GET wit/comments"
	routeCommentCreate  = "devops invoke POST wit/comments"
	routeCommentEdit    = "devops invoke PATCH wit/comments"
	routeQuery          = "boards query"
	routeTagUpdate      = "boards work-item update"
)

// extensionCalled returns the recorded extension commands of one route.
func (f *fakeAz) extensionCalled(route string) []azuredevopstest.ExtensionCall {
	matched := []azuredevopstest.ExtensionCall{}
	for _, call := range f.extensionCalls {
		if call.Route() == route {
			matched = append(matched, call)
		}
	}
	return matched
}

// batchOf wraps work item JSON objects as a workItemsBatch response.
func batchOf(items ...string) string {
	return `{"count":` + strconv.Itoa(len(items)) + `,"value":[` + strings.Join(items, ",") + `]}`
}

// boardsItem returns one work item of the registered project.
func boardsItem(id, state, tags string) string {
	return `{"id":` + id + `,"fields":{"System.TeamProject":"Factory Pilot","System.Title":"t","System.WorkItemType":"Product Backlog Item","System.State":"` + state + `","System.Tags":"` + tags + `"}}`
}

// pbiStates is the state list of the Scrum Product Backlog Item type.
const pbiStates = `{"count":5,"value":[{"name":"New","category":"Proposed"},{"name":"Approved","category":"Proposed"},{"name":"Committed","category":"InProgress"},{"name":"Done","category":"Completed"},{"name":"Removed","category":"Removed"}]}`

// assignedTag is the assignment tag of the registered repository.
var assignedTag = "factory-repo:" + azuredevopstest.RepositoryID

// TestExtensionIssueReadsTheWorkItemInsideTheRegisteredProject verifies that
// the extension route reads a work item through the project-scoped batch
// resource, keeps a Markdown description, and maps the state category.
func TestExtensionIssueReadsTheWorkItemInsideTheRegisteredProject(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on(routeWorkItemsBatch, `{"count":1,"value":[{"id":42,"fields":{"System.TeamProject":"Factory Pilot","System.Title":"Export","System.WorkItemType":"Product Backlog Item","System.State":"Committed","System.Tags":"agent-running; `+assignedTag+`","System.Description":"## Goal\n\n- a < b"},"multilineFieldsFormat":{"System.Description":"markdown"}}]}`).
		on(routeTypeStates, pbiStates)

	issue, err := extensionClient(az).Issue(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if issue.Number != 42 || issue.Body != "## Goal\n\n- a < b" || issue.State != "open" || len(issue.Labels) != 2 || issue.Labels[1] != assignedTag {
		t.Fatalf("Issue() = %#v", issue)
	}
	batch := az.extensionCalled(routeWorkItemsBatch)[0]
	if batch.Parameters("--route-parameters")["project"] != "Factory Pilot" {
		t.Fatalf("batch route parameters = %v, want the registered project", batch.Flags["--route-parameters"])
	}
	var body struct {
		IDs         []int  `json:"ids"`
		ErrorPolicy string `json:"errorPolicy"`
	}
	if err := json.Unmarshal([]byte(batch.Body), &body); err != nil || len(body.IDs) != 1 || body.IDs[0] != 42 || body.ErrorPolicy != "omit" {
		t.Fatalf("batch body = %q, want ids [42] with errorPolicy omit", batch.Body)
	}
	states := az.extensionCalled(routeTypeStates)[0].Parameters("--route-parameters")
	if states["project"] != "Factory Pilot" || states["type"] != "Product Backlog Item" {
		t.Fatalf("states route parameters = %v", states)
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
}

// TestExtensionIssueRefusesAWorkItemOfAnotherProject verifies that a work
// item outside the registered project, or a missing one, is never read as an
// issue of the registration.
func TestExtensionIssueRefusesAWorkItemOfAnotherProject(t *testing.T) {
	t.Parallel()

	for name, response := range map[string]string{
		"other project": `{"count":1,"value":[{"id":42,"fields":{"System.TeamProject":"Other","System.WorkItemType":"Product Backlog Item","System.State":"New"}}]}`,
		"missing":       `{"count":1,"value":[null]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			az := newFakeAz(t).on(routeWorkItemsBatch, response).on(routeTypeStates, pbiStates)
			if _, err := extensionClient(az).Issue(context.Background(), repository, 42); err == nil || !strings.Contains(err.Error(), "42") {
				t.Fatalf("Issue() error = %v, want a refusal naming work item 42", err)
			}
		})
	}
}

// TestExtensionIssueHidesAnAgentReadyTagThatAnUnauthorizedUserAdded verifies
// that the update history is read through invoke with paging, and that the
// agent-ready tag counts only when an authorized user added it last.
func TestExtensionIssueHidesAnAgentReadyTagThatAnUnauthorizedUserAdded(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on(routeWorkItemsBatch, batchOf(boardsItem("42", "New", "agent-ready; "+assignedTag))).
		on(routeTypeStates, pbiStates).
		on(routeUpdates, `{"count":2,"value":[
			{"revisedBy":{"uniqueName":"alice@contoso.com"},"fields":{"System.Tags":{"newValue":"agent-ready"}}},
			{"revisedBy":{"uniqueName":"mallory@contoso.com"},"fields":{"System.Tags":{"oldValue":"","newValue":"agent-ready; `+assignedTag+`"}}}]}`)

	issue, err := extensionClient(az).Issue(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	for _, label := range issue.Labels {
		if label == tracker.LabelAgentReady {
			t.Fatalf("labels = %v, want agent-ready hidden", issue.Labels)
		}
	}
	updates := az.extensionCalled(routeUpdates)[0]
	if route := updates.Parameters("--route-parameters"); route["project"] != "Factory Pilot" || route["id"] != "42" {
		t.Fatalf("updates route parameters = %v", route)
	}
	if query := updates.Parameters("--query-parameters"); query["$top"] != "200" || query["$skip"] != "0" {
		t.Fatalf("updates query parameters = %v", query)
	}
}

// repositoryShow returns the az repos show response of one scripted
// repository of the shared project.
func repositoryShow(name, id string) string {
	return `{"id":"` + id + `","name":"` + name + `","project":{"id":"` + azuredevopstest.ProjectID + `","name":"Factory Pilot"}}`
}

// routeReposList is the routing key of az repos list.
const routeReposList = "repos list"

// projectRepositoryList is the az repos list response of the shared
// project: a JSON array of both scripted repositories.
var projectRepositoryList = "[" + repositoryShow("service", azuredevopstest.RepositoryID) + "," + repositoryShow("web", azuredevopstest.WebRepositoryID) + "]"

// sharedProjectQueue scripts one installation's view of the shared project:
// work item 3 is assigned to service, 5 to web, 7 has no assignment, and 9
// has two assignments. Alice added every agent-ready tag.
func sharedProjectQueue(t *testing.T, name, id string) *fakeAz {
	t.Helper()
	webTag := "factory-repo:" + azuredevopstest.WebRepositoryID
	return newFakeAz(t).
		on("repos show", repositoryShow(name, id)).
		on(routeQuery, `[{"id":9,"fields":{"System.Id":9}},{"id":3,"fields":{"System.Id":3}},{"id":5,"fields":{"System.Id":5}},{"id":7,"fields":{"System.Id":7}}]`).
		on(routeWorkItemsBatch, batchOf(
			boardsItem("3", "New", "agent-ready; "+assignedTag),
			boardsItem("5", "New", "agent-ready; "+webTag),
			boardsItem("7", "New", "agent-ready"),
			boardsItem("9", "New", "agent-ready; "+assignedTag+"; "+webTag))).
		on(routeTypeStates, pbiStates).
		on(routeUpdates, `{"count":1,"value":[{"revisedBy":{"uniqueName":"alice@contoso.com"},"fields":{"System.Tags":{"newValue":"agent-ready"}}}]}`)
}

// TestExtensionQueueListsOnlyWorkItemsAssignedToTheRegisteredRepository
// verifies that two installations polling one shared project each see only
// the PBIs whose single assignment tag names their own repository GUID, and
// that the WIQL names the project explicitly instead of @project.
func TestExtensionQueueListsOnlyWorkItemsAssignedToTheRegisteredRepository(t *testing.T) {
	t.Parallel()

	for _, registration := range []struct {
		name, id string
		want     int
	}{
		{name: azuredevopstest.Repository, id: azuredevopstest.RepositoryID, want: 3},
		{name: azuredevopstest.WebRepository, id: azuredevopstest.WebRepositoryID, want: 5},
	} {
		az := sharedProjectQueue(t, registration.name, registration.id)
		target := tracker.Repository{Owner: "contoso", Project: "Factory Pilot", Name: registration.name}
		issues, err := extensionClient(az).ListEligibleIssues(context.Background(), target)
		if err != nil {
			t.Fatalf("%s: ListEligibleIssues() error = %v", registration.name, err)
		}
		if len(issues) != 1 || issues[0].Number != registration.want {
			t.Fatalf("%s: ListEligibleIssues() = %#v, want only work item %d", registration.name, issues, registration.want)
		}
		query := az.extensionCalled(routeQuery)[0]
		wiql := query.Flag("--wiql")
		if query.Flag("--project") != "Factory Pilot" || !strings.Contains(wiql, "[System.TeamProject] = 'Factory Pilot'") || strings.Contains(wiql, "@project") ||
			!strings.Contains(wiql, "CONTAINS 'agent-ready'") || !strings.Contains(wiql, "CONTAINS 'factory-repo:"+registration.id+"'") {
			t.Fatalf("%s: query = %+v", registration.name, query)
		}
	}
}

// TestClaimRepositoryAssignmentRefusesEveryInvalidAssignment verifies the
// authoritative claim check: only exactly one tag naming the registered
// repository GUID is accepted, and every refusal names the tag to set.
func TestClaimRepositoryAssignmentRefusesEveryInvalidAssignment(t *testing.T) {
	t.Parallel()

	webTag := "factory-repo:" + azuredevopstest.WebRepositoryID
	for name, tc := range map[string]struct {
		labels  []string
		problem azuredevops.AssignmentProblem
	}{
		"missing":    {[]string{tracker.LabelAgentReady}, azuredevops.AssignmentMissing},
		"malformed":  {[]string{tracker.LabelAgentReady, "factory-repo:service"}, azuredevops.AssignmentMalformed},
		"multiple":   {[]string{tracker.LabelAgentReady, assignedTag, webTag}, azuredevops.AssignmentMultiple},
		"mismatched": {[]string{tracker.LabelAgentReady, webTag}, azuredevops.AssignmentMismatched},
		"unknown":    {[]string{tracker.LabelAgentReady, "factory-repo:00000000-0000-4000-8000-000000000000"}, azuredevops.AssignmentUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			az := newFakeAz(t).
				on("repos show", repositoryShow("service", azuredevopstest.RepositoryID)).
				on(routeReposList, projectRepositoryList)
			_, err := extensionClient(az).ClaimRepositoryAssignment(context.Background(), repository, tracker.Issue{Number: 42, Labels: tc.labels})
			if err == nil || !strings.Contains(err.Error(), assignedTag) {
				t.Fatalf("ClaimRepositoryAssignment() error = %v, want a refusal naming %s", err, assignedTag)
			}
			var refused *azuredevops.AssignmentError
			if !errors.As(err, &refused) || refused.Problem != tc.problem {
				t.Fatalf("ClaimRepositoryAssignment() error = %v, want problem %q", err, tc.problem)
			}
			if listed := az.extensionCalled(routeReposList); len(listed) > 1 || (len(listed) == 1 && listed[0].Flag("--project") != "Factory Pilot") {
				t.Fatalf("repos list calls = %+v, want at most one for project Factory Pilot", listed)
			}
		})
	}
}

// TestClaimRepositoryAssignmentReturnsTheValidatedIdentity verifies that an
// accepted assignment returns the stable identities that the run freezes.
func TestClaimRepositoryAssignmentReturnsTheValidatedIdentity(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("repos show", repositoryShow("service", azuredevopstest.RepositoryID))
	assignment, err := extensionClient(az).ClaimRepositoryAssignment(context.Background(), repository, tracker.Issue{Number: 42, Labels: []string{tracker.LabelAgentReady, assignedTag}})
	if err != nil {
		t.Fatalf("ClaimRepositoryAssignment() error = %v", err)
	}
	want := tracker.RepositoryAssignment{Organization: "contoso", ProjectID: azuredevopstest.ProjectID, Project: "Factory Pilot", RepositoryID: azuredevopstest.RepositoryID, Repository: "service"}
	if assignment == nil || *assignment != want {
		t.Fatalf("ClaimRepositoryAssignment() = %+v, want %+v", assignment, want)
	}
}

// TestExtensionReplaceIssueLabelsKeepsAssignmentAndCorrelationTags verifies
// that a lifecycle tag update goes through az boards work-item update after
// a project-scoped read, and keeps the factory-repo and factory-plan tags
// even when the caller's label list omits them.
func TestExtensionReplaceIssueLabelsKeepsAssignmentAndCorrelationTags(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on(routeWorkItemsBatch, batchOf(boardsItem("42", "New", "agent-ready; factory-plan:s/1/a; "+assignedTag))).
		on(routeTagUpdate, `{"id":42}`)
	if err := extensionClient(az).ReplaceIssueLabels(context.Background(), repository, 42, []string{"backend", tracker.LabelAgentRunning}); err != nil {
		t.Fatalf("ReplaceIssueLabels() error = %v", err)
	}
	update := az.extensionCalled(routeTagUpdate)
	if len(update) != 1 || update[0].Flag("--id") != "42" {
		t.Fatalf("updates = %+v, want one update of work item 42", update)
	}
	want := "System.Tags=backend; agent-running; factory-plan:s/1/a; " + assignedTag
	if fields := update[0].Flags["--fields"]; len(fields) != 1 || fields[0] != want {
		t.Fatalf("--fields = %q, want %q", fields, want)
	}
}

// TestExtensionReplaceIssueLabelsRefusesAWorkItemOfAnotherProject verifies
// that the organization-scoped update never runs for a work item outside the
// registered project.
func TestExtensionReplaceIssueLabelsRefusesAWorkItemOfAnotherProject(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on(routeWorkItemsBatch, `{"count":1,"value":[{"id":42,"fields":{"System.TeamProject":"Other","System.Tags":"agent-ready"}}]}`).
		on(routeTagUpdate, `{"id":42}`)
	if err := extensionClient(az).ReplaceIssueLabels(context.Background(), repository, 42, []string{tracker.LabelAgentRunning}); err == nil {
		t.Fatal("ReplaceIssueLabels() error = nil, want a refusal")
	}
	if len(az.extensionCalled(routeTagUpdate)) != 0 {
		t.Fatal("the tag update ran for a work item of another project")
	}
}

// TestExtensionIssueCommentsReadsEveryPageThroughInvoke verifies that the
// comment list uses the comments resource at an API version invoke can
// parse, follows the continuation token that invoke copies from the
// response header, and skips deleted comments.
func TestExtensionIssueCommentsReadsEveryPageThroughInvoke(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on(routeCommentsList,
		`{"comments":[{"id":3,"text":"/factory approve","format":"markdown","createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T08:00:00Z"},
			{"id":4,"text":"gone","isDeleted":true,"createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T08:02:00Z"}],
		  "continuation_token":"next"}`,
		`{"comments":[{"id":12,"text":"<div>/factory status</div>","createdBy":{"uniqueName":"bob@contoso.com"},"createdDate":"2026-10-09T09:30:00Z"}],"continuation_token":null}`)

	comments, err := extensionClient(az).IssueComments(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("IssueComments() error = %v", err)
	}
	if len(comments) != 2 || comments[0].ID != "20261009T080000.000000000Z.w42.c3" || comments[1].Body != "/factory status" || comments[1].Author != "bob@contoso.com" {
		t.Fatalf("IssueComments() = %#v", comments)
	}
	calls := az.extensionCalled(routeCommentsList)
	if len(calls) != 2 {
		t.Fatalf("comment pages = %d, want 2", len(calls))
	}
	first, second := calls[0], calls[1]
	if first.Flag("--api-version") != "7.1-preview" || first.Parameters("--route-parameters")["workItemId"] != "42" || first.Parameters("--route-parameters")["project"] != "Factory Pilot" {
		t.Fatalf("first page = %+v", first)
	}
	if _, ok := first.Parameters("--query-parameters")["continuationToken"]; ok || second.Parameters("--query-parameters")["continuationToken"] != "next" {
		t.Fatalf("continuation = %v then %v", first.Flags["--query-parameters"], second.Flags["--query-parameters"])
	}
}

// TestExtensionStatusCommentIsCreatedAndEditedAsMarkdown verifies the
// Markdown create and edit through invoke, with the body only in the
// request file.
func TestExtensionStatusCommentIsCreatedAndEditedAsMarkdown(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on(routeCommentCreate, `{"id":15,"text":"## Status","format":"markdown","createdBy":{"uniqueName":"factory@contoso.com"},"createdDate":"2026-10-09T10:00:00Z"}`).
		on(routeCommentEdit, `{"id":15,"text":"updated","format":"markdown","createdBy":{"uniqueName":"factory@contoso.com"},"createdDate":"2026-10-09T10:00:00Z"}`)
	client := extensionClient(az)

	created, err := client.CreateIssueComment(context.Background(), repository, 42, "## Status")
	if err != nil || created.ID != "20261009T100000.000000000Z.w42.c15" || created.Body != "## Status" {
		t.Fatalf("CreateIssueComment() = %#v, %v", created, err)
	}
	create := az.extensionCalled(routeCommentCreate)[0]
	if create.Parameters("--query-parameters")["format"] != "markdown" || create.Body != `{"text":"## Status"}` || create.Flag("--api-version") != "7.1-preview" {
		t.Fatalf("create = %+v body %q", create, create.Body)
	}
	if err := client.EditIssueComment(context.Background(), repository, created.ID, "updated"); err != nil {
		t.Fatalf("EditIssueComment() error = %v", err)
	}
	edit := az.extensionCalled(routeCommentEdit)[0]
	if route := edit.Parameters("--route-parameters"); route["workItemId"] != "42" || route["commentId"] != "15" || edit.Body != `{"text":"updated"}` || edit.Parameters("--query-parameters")["format"] != "markdown" {
		t.Fatalf("edit = %+v body %q", edit, edit.Body)
	}
}

// TestExtensionCommentWriteRefusesAnHTMLResult verifies that a comment the
// service did not store as Markdown fails loudly: the comment API version
// without a resource version is not verified live (ADR 0030), so the adapter
// checks the stored format instead of assuming it.
func TestExtensionCommentWriteRefusesAnHTMLResult(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on(routeCommentCreate, `{"id":15,"text":"## Status","format":"html","createdDate":"2026-10-09T10:00:00Z"}`)
	if _, err := extensionClient(az).CreateIssueComment(context.Background(), repository, 42, "## Status"); err == nil || !strings.Contains(err.Error(), "Markdown") {
		t.Fatalf("CreateIssueComment() error = %v, want a refused format", err)
	}
}

// TestExtensionFindStatusCommentMatchesTheOneMarkedComment verifies the
// extension-route recovery of the status comment without a signed-in
// identity: exactly one live comment with the marker is recovered, none is a
// zero comment, and more than one is an error, never a duplicate create.
func TestExtensionFindStatusCommentMatchesTheOneMarkedComment(t *testing.T) {
	t.Parallel()

	marker := "<!-- factory-status: run-1 -->"
	comment := func(id, text string) string {
		return `{"id":` + id + `,"text":"` + text + `","format":"markdown","createdBy":{"uniqueName":"factory@contoso.com"},"createdDate":"2026-10-09T10:00:00Z"}`
	}
	tests := []struct {
		name, response string
		wantID         string
		wantErr        bool
	}{
		{name: "one", response: `{"comments":[` + comment("3", "hello") + `,` + comment("15", "## Status "+marker) + `]}`, wantID: "20261009T100000.000000000Z.w42.c15"},
		{name: "none", response: `{"comments":[` + comment("3", "hello") + `]}`},
		{name: "deleted copy", response: `{"comments":[{"id":8,"text":"` + marker + `","isDeleted":true},` + comment("15", marker) + `]}`, wantID: "20261009T100000.000000000Z.w42.c15"},
		{name: "two", response: `{"comments":[` + comment("8", "copied "+marker) + `,` + comment("15", marker) + `]}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			az := newFakeAz(t).on(routeCommentsList, tc.response)
			found, err := extensionClient(az).FindStatusComment(context.Background(), repository, 42, marker)
			if (err != nil) != tc.wantErr || found.ID != tc.wantID {
				t.Fatalf("FindStatusComment() = %#v, %v; want id %q, error %t", found, err, tc.wantID, tc.wantErr)
			}
		})
	}
}

// TestAzRestRouteNeedsNoRepositoryAssignment verifies that the az rest route
// keeps its claim behavior: no assignment, and no command.
func TestAzRestRouteNeedsNoRepositoryAssignment(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t)
	assignment, err := newClient(az).ClaimRepositoryAssignment(context.Background(), repository, tracker.Issue{Number: 42})
	if err != nil || assignment != nil || len(az.calls)+len(az.extensionCalls) != 0 {
		t.Fatalf("ClaimRepositoryAssignment() = %+v, %v with %d calls, want nil without calls", assignment, err, len(az.calls)+len(az.extensionCalls))
	}
}
