package azuredevops_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestIssueReadsAWorkItemAsAnIssueSnapshot verifies the work item fields, the
// tag list, the open state from the state category, and the conversion of an
// HTML description to text.
func TestIssueReadsAWorkItemAsAnIssueSnapshot(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("GET "+projectPath+"/wit/workitems/42", `{"id":42,"fields":{
			"System.Title":"Add export",
			"System.WorkItemType":"User Story",
			"System.State":"Active",
			"System.Tags":"agent-running; backend",
			"System.ChangedDate":"2026-10-09T08:15:00.123Z",
			"System.Description":"<div>Export the <b>report</b> as CSV.</div><ul><li>one &amp; two</li></ul><div>&lt;!-- factory-route: fix --&gt;</div><!-- factory-route: fix -->"}}`).
		on("GET "+projectPath+"/wit/workitemtypes/User Story/states", userStoryStates)

	issue, err := newClient(az).Issue(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	want := tracker.Issue{
		Number:    42,
		Title:     "Add export",
		Body:      "Export the report as CSV.\n- one & two\n<!-- factory-route: fix -->\n<!-- factory-route: fix -->",
		State:     "open",
		Labels:    []string{"agent-running", "backend"},
		UpdatedAt: time.Date(2026, 10, 9, 8, 15, 0, 123000000, time.UTC),
	}
	if issue.Number != want.Number || issue.Title != want.Title || issue.Body != want.Body || issue.State != want.State || !issue.UpdatedAt.Equal(want.UpdatedAt) || len(issue.Labels) != 2 || issue.Labels[0] != "agent-running" || issue.Labels[1] != "backend" {
		t.Fatalf("Issue() = %#v, want %#v", issue, want)
	}
}

// TestIssueKeepsAMarkdownDescriptionAndReportsADoneStateAsClosed verifies
// that a Markdown description passes unchanged and that a state in the
// Completed category maps to closed.
func TestIssueKeepsAMarkdownDescriptionAndReportsADoneStateAsClosed(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("GET "+projectPath+"/wit/workitems/7", `{"id":7,"fields":{
			"System.Title":"Done",
			"System.WorkItemType":"User Story",
			"System.State":"Closed",
			"System.Description":"## Route\n\n<!-- factory-route: fix -->\n- a < b"},
			"multilineFieldsFormat":{"System.Description":"markdown"}}`).
		on("GET "+projectPath+"/wit/workitemtypes/User Story/states", userStoryStates)

	issue, err := newClient(az).Issue(context.Background(), repository, 7)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if issue.Body != "## Route\n\n<!-- factory-route: fix -->\n- a < b" || issue.State != "closed" || len(issue.Labels) != 0 {
		t.Fatalf("Issue() = %#v, want the Markdown body unchanged, closed, and no labels", issue)
	}
}

// TestListEligibleIssuesReturnsOpenWorkItemsThatAnAuthorizedUserTagged
// verifies the WIQL query, the open-state filter, and the authorization of
// the identity that last added the agent-ready tag.
func TestListEligibleIssuesReturnsOpenWorkItemsThatAnAuthorizedUserTagged(t *testing.T) {
	t.Parallel()

	workItem := func(id int, state, tags string) string {
		return `{"id":` + strconv.Itoa(id) + `,"fields":{"System.Title":"t","System.WorkItemType":"User Story","System.State":"` + state + `","System.Tags":"` + tags + `"}}`
	}
	az := newFakeAz(t).
		on("POST "+projectPath+"/wit/wiql", `{"workItems":[{"id":9},{"id":5},{"id":3}]}`).
		on("GET "+projectPath+"/wit/workitems", `{"value":[`+workItem(9, "Closed", "agent-ready")+`,`+workItem(5, "Active", "agent-ready")+`,`+workItem(3, "New", "backend; agent-ready")+`]}`).
		on("GET "+projectPath+"/wit/workitemtypes/User Story/states", userStoryStates).
		on("GET "+projectPath+"/wit/workItems/3/updates", `{"value":[
			{"revisedBy":{"uniqueName":"bob@contoso.com"},"fields":{"System.Tags":{"newValue":"backend"}}},
			{"revisedBy":{"uniqueName":"Alice@Contoso.com"},"fields":{"System.Tags":{"oldValue":"backend","newValue":"backend; agent-ready"}}},
			{"revisedBy":{"uniqueName":"bob@contoso.com"},"fields":{"System.Rev":{"oldValue":2,"newValue":3},"System.Title":{"newValue":"t"}}}]}`).
		on("GET "+projectPath+"/wit/workItems/5/updates", `{"value":[
			{"revisedBy":{"uniqueName":"alice@contoso.com"},"fields":{"System.Tags":{"newValue":"agent-ready"}}},
			{"revisedBy":{"uniqueName":"mallory@contoso.com"},"fields":{"System.Tags":{"oldValue":"agent-ready","newValue":""}}},
			{"revisedBy":{"uniqueName":"mallory@contoso.com"},"fields":{"System.Tags":{"oldValue":"","newValue":"agent-ready"}}}]}`)

	issues, err := newClient(az).ListEligibleIssues(context.Background(), repository)
	if err != nil {
		t.Fatalf("ListEligibleIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 3 || issues[0].State != "open" {
		t.Fatalf("ListEligibleIssues() = %#v, want only work item 3", issues)
	}
	var query struct {
		Query string `json:"query"`
	}
	decodeBody(t, az.called("POST " + projectPath + "/wit/wiql")[0], &query)
	if !strings.Contains(query.Query, "[System.Tags] CONTAINS 'agent-ready'") || !strings.Contains(query.Query, "[System.TeamProject] = @project") {
		t.Fatalf("WIQL = %q, want a project query for the agent-ready tag", query.Query)
	}
	if ids := az.called("GET " + projectPath + "/wit/workitems")[0].url.Query().Get("ids"); ids != "3,5,9" {
		t.Fatalf("batch ids = %q, want 3,5,9", ids)
	}
}

// TestReplaceIssueLabelsSetsTheTagField verifies the JSON Patch that replaces
// the complete tag list, and that a tag needs no creation step.
func TestReplaceIssueLabelsSetsTheTagField(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("PATCH "+projectPath+"/wit/workitems/42", `{"id":42}`)
	client := newClient(az)
	if err := client.CreateLabel(context.Background(), repository, tracker.Label{Name: tracker.LabelAgentRunning}); err != nil {
		t.Fatalf("CreateLabel() error = %v", err)
	}
	if err := client.ReplaceIssueLabels(context.Background(), repository, 42, []string{"backend", tracker.LabelAgentRunning}); err != nil {
		t.Fatalf("ReplaceIssueLabels() error = %v", err)
	}
	if len(az.calls) != 1 {
		t.Fatalf("calls = %d, want only the tag patch", len(az.calls))
	}
	call := az.calls[0]
	var patch []map[string]string
	decodeBody(t, call, &patch)
	if call.headers != "Content-Type=application/json-patch+json" || len(patch) != 1 || patch[0]["op"] != "add" || patch[0]["path"] != "/fields/System.Tags" || patch[0]["value"] != "backend; agent-running" {
		t.Fatalf("patch = %s %#v, want one System.Tags replacement", call.headers, patch)
	}
}

// TestIssueCommentsReadsEveryPageAsTimeOrderedComments verifies paging, the
// HTML and Markdown comment formats, deleted comments, and identities that
// sort in creation order.
func TestIssueCommentsReadsEveryPageAsTimeOrderedComments(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+projectPath+"/wit/workItems/42/comments",
		`{"comments":[
			{"id":3,"text":"<div>/factory approve</div>","createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T08:00:00Z","modifiedDate":"2026-10-09T08:01:00Z"},
			{"id":4,"text":"gone","isDeleted":true,"createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T08:02:00Z"}],
		  "continuationToken":"next"}`,
		`{"comments":[
			{"id":12,"text":"/factory status\n<!-- x -->","format":"markdown","createdBy":{"uniqueName":"bob@contoso.com"},"createdDate":"2026-10-09T09:30:00.5Z","modifiedDate":"2026-10-09T09:30:00.5Z"}]}`)

	comments, err := newClient(az).IssueComments(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("IssueComments() error = %v", err)
	}
	want := []tracker.Comment{
		{ID: "20261009T080000.000000000Z.w42.c3", Body: "/factory approve", Author: "alice@contoso.com", UpdatedAt: time.Date(2026, 10, 9, 8, 1, 0, 0, time.UTC)},
		{ID: "20261009T093000.500000000Z.w42.c12", Body: "/factory status\n<!-- x -->", Author: "bob@contoso.com", UpdatedAt: time.Date(2026, 10, 9, 9, 30, 0, 500000000, time.UTC)},
	}
	if len(comments) != len(want) {
		t.Fatalf("IssueComments() = %#v, want %#v", comments, want)
	}
	for index := range want {
		if comments[index].ID != want[index].ID || comments[index].Body != want[index].Body || comments[index].Author != want[index].Author || !comments[index].UpdatedAt.Equal(want[index].UpdatedAt) {
			t.Fatalf("comment %d = %#v, want %#v", index, comments[index], want[index])
		}
	}
	calls := az.called("GET " + projectPath + "/wit/workItems/42/comments")
	if len(calls) != 2 || calls[1].url.Query().Get("continuationToken") != "next" || calls[0].url.Query().Get("api-version") != "7.1-preview.4" {
		t.Fatalf("comment calls = %#v, want two pages of the preview API", calls)
	}
}

// TestStatusCommentIsCreatedFoundAndEditedAsMarkdown verifies the comment
// format, that only a comment of the authenticated identity is recovered by
// its marker, and that an edit addresses the work item from the identity.
func TestStatusCommentIsCreatedFoundAndEditedAsMarkdown(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("POST "+projectPath+"/wit/workItems/42/comments", `{"id":15,"workItemId":42,"text":"body","createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T10:00:00Z","modifiedDate":"2026-10-09T10:00:00Z"}`).
		on("GET /contoso/_apis/connectionData", connectionData).
		on("GET "+projectPath+"/wit/workItems/42/comments", `{"comments":[
			{"id":8,"text":"copied <!-- factory-status: run-1 -->","format":"markdown","createdBy":{"uniqueName":"mallory@contoso.com"},"createdDate":"2026-10-09T09:00:00Z"},
			{"id":15,"text":"## Status\n<!-- factory-status: run-1 -->","format":"markdown","createdBy":{"uniqueName":"alice@contoso.com"},"createdDate":"2026-10-09T10:00:00Z"}]}`).
		on("PATCH "+projectPath+"/wit/workItems/42/comments/15", `{"id":15}`)
	client := newClient(az)

	created, err := client.CreateIssueComment(context.Background(), repository, 42, "## Status\n<!-- factory-status: run-1 -->")
	if err != nil || created.ID != "20261009T100000.000000000Z.w42.c15" {
		t.Fatalf("CreateIssueComment() = %#v/%v, want the time-ordered identity", created, err)
	}
	create := az.called("POST " + projectPath + "/wit/workItems/42/comments")[0]
	var text map[string]string
	decodeBody(t, create, &text)
	if create.url.Query().Get("format") != "markdown" || text["text"] != "## Status\n<!-- factory-status: run-1 -->" {
		t.Fatalf("create = %s %#v, want a Markdown comment", create.url, text)
	}
	found, err := client.FindStatusComment(context.Background(), repository, 42, "<!-- factory-status: run-1 -->")
	if err != nil || found.ID != created.ID {
		t.Fatalf("FindStatusComment() = %#v/%v, want the comment of the authenticated identity", found, err)
	}
	if err := client.EditIssueComment(context.Background(), repository, found.ID, "updated"); err != nil {
		t.Fatalf("EditIssueComment() error = %v", err)
	}
	edit := az.called("PATCH " + projectPath + "/wit/workItems/42/comments/15")[0]
	decodeBody(t, edit, &text)
	if edit.url.Query().Get("format") != "markdown" || text["text"] != "updated" {
		t.Fatalf("edit = %s %#v, want a Markdown edit", edit.url, text)
	}
	if err := client.EditIssueComment(context.Background(), repository, "15", "x"); err == nil {
		t.Fatal("EditIssueComment() with a foreign identity error = nil, want a rejection")
	}
}
